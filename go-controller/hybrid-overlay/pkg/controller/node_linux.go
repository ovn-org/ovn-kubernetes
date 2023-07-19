package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

const (
	extBridgeName string = "br-ext"
	extVXLANName  string = "ext-vxlan"
)

type flowCacheEntry struct {
	flows []string
	// special table 20 flow if it has been learned from the switch
	learnedFlow string
	// ignore learn on next flow sync for this entry
	ignoreLearn bool
}

// NodeController is the node hybrid overlay controller
type NodeController struct {
	nodeName  string
	initState hotypes.HybridInitState
	drMAC     net.HardwareAddr
	drIP      net.IP
	gwLRPIP   net.IP
	vxlanPort uint16
	// contains a map of pods to corresponding tunnels
	flowCache map[string]*flowCacheEntry
	flowMutex sync.Mutex
	// channel to indicate we need to update flows immediately
	flowChan            chan struct{}
	flowCacheSyncPeriod time.Duration

	nodeLister     listers.NodeLister
	localPodLister listers.PodLister
}

// newNodeController returns a node handler that listens for node events
// so that Add/Update/Delete events are appropriately handled.
// It initializes the node it is currently running on. On Linux, this means:
//  1. Setting up a VXLAN gateway and hooking to the OVN gateway
//  2. Setting back annotations about its VTEP and gateway MAC address to its own object
func newNodeController(
	_ kube.Interface,
	nodeName string,
	nodeLister listers.NodeLister,
	localPodLister listers.PodLister,
) (nodeController, error) {

	node := &NodeController{
		nodeName:            nodeName,
		initState:           new(uint32),
		vxlanPort:           uint16(config.HybridOverlay.VXLANPort),
		flowCache:           make(map[string]*flowCacheEntry),
		flowMutex:           sync.Mutex{},
		flowChan:            make(chan struct{}, 1),
		flowCacheSyncPeriod: 30 * time.Second,
		nodeLister:          nodeLister,
		localPodLister:      localPodLister,
	}
	atomic.StoreUint32(node.initState, hotypes.InitialStartup)
	return node, nil
}

func podIPToCookie(podIP net.IP) string {
	//TODO add ipv6 support
	ip4 := podIP.To4()
	if ip4 == nil {
		return ""
	}
	return fmt.Sprintf("%02x%02x%02x%02x", ip4[0], ip4[1], ip4[2], ip4[3])
}

// AddPod handles the pod add event
func (n *NodeController) AddPod(pod *kapi.Pod) error {
	// nothing to do for hostnetworked pod
	if util.PodWantsHostNetwork(pod) {
		return nil
	}
	if util.PodCompleted(pod) {
		klog.Infof("Cleaning up hybrid overlay pod %s/%s because it has completed", pod.Namespace, pod.Name)
		return n.DeletePod(pod)
	}
	podIPs, podMAC, err := getPodDetails(pod)
	if err != nil {
		klog.Warningf("Cleaning up hybrid overlay pod %s/%s because it has no OVN annotation %v", pod.Namespace, pod.Name, err)
		return n.DeletePod(pod)
	}

	// It's always safe to ignore the learn flow as we only process and add or update
	// if the IP/MAC or Annotations have changed
	ignoreLearn := true

	if n.drMAC == nil || n.drIP == nil {
		return fmt.Errorf("empty values for DR MAC: %s or DR IP: %s on node %s", n.drMAC, n.drIP, n.nodeName)
	}

	for _, podIP := range podIPs {
		var flows []string
		cookie := podIPToCookie(podIP.IP)
		if cookie == "" {
			continue
		}
		// table 10 is pod dispatch - Incoming vxlan traffic towards pods
		flows = append(flows, fmt.Sprintf(
			"table=10,cookie=0x%s,priority=100,ip,nw_dst=%s,"+
				"actions=set_field:%s->eth_src,set_field:%s->eth_dst,output:ext",
			cookie, podIP.IP, n.drMAC.String(), podMAC))

		n.updateFlowCacheEntry(cookie, flows, ignoreLearn)
	}
	n.requestFlowSync()
	klog.Infof("Pod %s wired for Hybrid Overlay", pod.Name)
	return nil
}

// DeletePod handles the pod delete event
func (n *NodeController) DeletePod(pod *kapi.Pod) error {
	// nothing to do for hostnetworked pods
	if util.PodWantsHostNetwork(pod) {
		return nil
	}
	podIPs, _, err := getPodDetails(pod)
	if err != nil {
		return fmt.Errorf("error getting pod details: %v", err)
	}
	for _, podIP := range podIPs {
		cookie := podIPToCookie(podIP.IP)
		if cookie == "" {
			continue
		}
		n.deleteFlowsByCookie(cookie)
	}
	return nil
}

// Sync is not needed but must be implemented to fulfill the interface
func (n *NodeController) Sync(objs []*kapi.Node) {}

func nameToCookie(nodeName string) string {
	hash := sha256.Sum256([]byte(nodeName))
	return fmt.Sprintf("%02x%02x%02x%02x", hash[0], hash[1], hash[2], hash[3])
}

// hybridOverlayNodeUpdate sets up or tears down VXLAN tunnels to hybrid overlay
// nodes in the cluster
func (n *NodeController) hybridOverlayNodeUpdate(node *kapi.Node) error {
	if !houtil.IsHybridOverlayNode(node) {
		return nil
	}

	cidr, nodeIP, drMAC, err := getNodeDetails(node)
	if cidr == nil || nodeIP == nil || drMAC == nil {
		klog.V(5).Infof("Cleaning up hybrid overlay resources for node %q because: %v", node.Name, err)
		return n.DeleteNode(node)
	}

	klog.Infof("Setting up hybrid overlay tunnel to node %s", node.Name)

	// (re)add flows for the node
	cookie := nameToCookie(node.Name)
	drMACRaw := strings.Replace(drMAC.String(), ":", "", -1)

	var flows []string
	// Distributed Router MAC ARP responder flow; responds to ARP requests by OVN for
	// any IP address within this node's assigned subnet and returns our hybrid overlay
	// port's MAC address.
	flows = append(flows,
		fmt.Sprintf("cookie=0x%s,table=0,priority=100,arp,in_port=ext,arp_tpa=%s,"+
			"actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],"+
			"mod_dl_src:%s,"+
			"load:0x2->NXM_OF_ARP_OP[],"+
			"move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],"+
			"load:0x%s->NXM_NX_ARP_SHA[],"+
			"move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],"+
			"move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],"+
			"move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],"+
			"IN_PORT",
			cookie, cidr.String(), drMAC.String(), drMACRaw))
	// Send all flows for the remote node's assigned subnet to that node via the VXLAN tunnel.
	// Windows hybrid overlay implementation requires that we set the destination MAC address
	// to the node's Distributed Router MAC.
	flows = append(flows,
		fmt.Sprintf("cookie=0x%s,table=0,priority=100,ip,nw_dst=%s,"+
			"actions=load:%d->NXM_NX_TUN_ID[0..31],"+
			"set_field:%s->tun_dst,"+
			"set_field:%s->eth_dst,"+
			"output:"+extVXLANName,
			cookie, cidr.String(), hotypes.HybridOverlayVNI, nodeIP.String(), drMAC.String()))

	flows = append(flows,
		fmt.Sprintf("cookie=0x%s,table=0,priority=101,ip,nw_dst=%s,nw_src=%s,"+
			"actions=load:%d->NXM_NX_TUN_ID[0..31],"+
			"set_field:%s->nw_src,"+
			"set_field:%s->tun_dst,"+
			"set_field:%s->eth_dst,"+
			"output:"+extVXLANName,
			cookie, cidr.String(), n.gwLRPIP.String(), hotypes.HybridOverlayVNI, n.drIP, nodeIP.String(), drMAC.String()))

	if len(config.HybridOverlay.ClusterSubnets) == 0 {
		// No static cluster subnet is provided in config. Try to detect the hybrid overlay node subnet dynamically
		// Add a route via the hybrid overlay port IP through the management port
		// interface for each hybrid overlay cluster subnet
		mgmtPortLink, err := util.GetNetLinkOps().LinkByName(types.K8sMgmtIntfName)
		if err != nil {
			return fmt.Errorf("failed to lookup link %s: %v", types.K8sMgmtIntfName, err)
		}

		route := makeRoute(cidr, n.drIP, mgmtPortLink)
		err = util.GetNetLinkOps().RouteAdd(route)
		if err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to add route for subnet %s via gateway %s: %v",
				route.Dst, route.Gw, err)
		}
	}

	n.updateFlowCacheEntry(cookie, flows, false)
	n.requestFlowSync()
	return nil
}

// AddNode handles node additions and updates
func (n *NodeController) AddNode(node *kapi.Node) error {
	klog.Info("Add Node ", node.Name)
	var err error
	if n.drIP == nil {
		hybridOverlayDRIP, ok := node.Annotations[hotypes.HybridOverlayDRIP]
		if !ok {
			return fmt.Errorf("hybrid overlay not initialized on %s, it was not assigned an interface address", node.Name)
		}
		n.drIP = net.ParseIP(hybridOverlayDRIP)
		if n.drIP == nil {
			return fmt.Errorf("hybrid overlay not initialized on %s, the the annotation %s = %s is not an IP address", node.Name, hotypes.HybridOverlayDRIP, hybridOverlayDRIP)
		}
	}

	if node.Name == n.nodeName {
		// Retry hybrid overlay initialization if the master was
		// slow to add the hybrid overlay logical network elements
		err = n.EnsureHybridOverlayBridge(node)
	} else {
		klog.Infof("Add hybridOverlay Node %s", node.Name)
		err = n.hybridOverlayNodeUpdate(node)
	}
	if atomic.LoadUint32(n.initState) == hotypes.DistributedRouterInitialized {
		pods, err := n.localPodLister.List(labels.Everything())
		if err != nil {
			return fmt.Errorf("cannot fully initialize node %s for hybrid overlay, cannot list pods: %v", n.nodeName, err)
		}
		for _, pod := range pods {
			err := n.AddPod(pod)
			if err != nil {
				klog.Errorf("Cannot wire pod %s for hybrid overlay, %v", pod.Name, err)
			}
		}
		atomic.StoreUint32(n.initState, hotypes.PodsInitialized)
	}
	return err
}

func (n *NodeController) deleteFlowsByCookie(cookie string) {
	n.flowMutex.Lock()
	defer n.flowMutex.Unlock()
	delete(n.flowCache, cookie)
}

// DeleteNode handles node deletions
func (n *NodeController) DeleteNode(node *kapi.Node) error {
	if node.Name == n.nodeName || !houtil.IsHybridOverlayNode(node) {
		return nil
	}

	n.deleteFlowsByCookie(nameToCookie(node.Name))

	cidr, _, _, err := getNodeDetails(node)
	if cidr == nil || err != nil {
		return fmt.Errorf("failed to lookup hybrid overlay node cidr for node %s: %v", node.Name, err)
	}
	if len(config.HybridOverlay.ClusterSubnets) == 0 {
		// No static cluster subnet is provided in config. Try to detect the hybrid overlay node subnet dynamically
		// Add a route via the hybrid overlay port IP through the management port
		// interface for each hybrid overlay cluster subnet
		mgmtPortLink, err := util.GetNetLinkOps().LinkByName(types.K8sMgmtIntfName)
		if err != nil {
			return fmt.Errorf("failed to lookup link %s: %v", types.K8sMgmtIntfName, err)
		}

		route := makeRoute(cidr, n.drIP, mgmtPortLink)
		err = util.GetNetLinkOps().RouteDel(route)
		if err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to delete route for subnet %s via gateway %s: %v",
				route.Dst, route.Gw, err)
		}
	}
	return nil
}

func getLocalNodeSubnet(nodeName string) (*net.IPNet, error) {
	var cidr string
	var err error

	// First wait for the node logical switch to be created by the Master, timeout is 300s.
	if err := wait.PollUntilContextTimeout(context.Background(), 500*time.Millisecond, 300*time.Second, true, func(ctx context.Context) (bool, error) {
		if cidr, _, err = util.RunOVNNbctl("get", "logical_switch", nodeName, "other-config:subnet"); err != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("timed out waiting for node %q logical switch: %v", nodeName, err)
	}

	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid hostsubnet found for node %s - %v", nodeName, err)
	}

	klog.Infof("Found node %s subnet %s", nodeName, subnet.String())
	return subnet, nil
}

func getIPAsHexString(ip net.IP) string {
	if ip.To4() != nil {
		ip = ip.To4()
	}
	asHex := ""
	for i := 0; i < len(ip); i++ {
		asHex += fmt.Sprintf("%02x", ip[i])
	}
	return asHex
}

// swaps out the new values of drIP and drMAC
func (n *NodeController) recalculateFlowCache(oldDRIP net.IP, oldDRMAC net.HardwareAddr) {
	n.flowMutex.Lock()
	defer n.flowMutex.Unlock()

	var ipReplace, macReplace int

	if oldDRIP != nil {
		ipReplace = -1
	}
	if oldDRMAC != nil {
		macReplace = -1
	}
	oldDRMACRaw := strings.Replace(oldDRMAC.String(), ":", "", -1)
	newDRMACRaw := strings.Replace(n.drMAC.String(), ":", "", -1)

	portIPRawOld := getIPAsHexString(oldDRIP)
	portIPRawNew := getIPAsHexString(n.drIP)

	for _, entry := range n.flowCache {
		for i, flow := range entry.flows {
			replacementFlow := flow
			replacementFlow = strings.Replace(replacementFlow, oldDRIP.String(), n.drIP.String(), ipReplace)
			replacementFlow = strings.Replace(replacementFlow, portIPRawOld, portIPRawNew, ipReplace)

			replacementFlow = strings.Replace(replacementFlow, oldDRMACRaw, newDRMACRaw, macReplace)
			replacementFlow = strings.Replace(replacementFlow, oldDRMAC.String(), n.drMAC.String(), macReplace)
			entry.flows[i] = replacementFlow

		}
	}
}

func (n *NodeController) createOrReplaceRoutes(mgmtPortLink netlink.Link, oldDRIP net.IP) error {
	// hybridOverlay has been initialized and will needs to delete the old routes
	if oldDRIP != nil {
		if len(config.HybridOverlay.ClusterSubnets) > 0 {
			for _, clusterEntry := range config.HybridOverlay.ClusterSubnets {
				route := makeRoute(clusterEntry.CIDR, oldDRIP, mgmtPortLink)
				err := util.GetNetLinkOps().RouteDel(route)
				if err != nil && !os.IsExist(err) {
					return fmt.Errorf("failed to delete route for subnet %s via gateway %s: %v",
						route.Dst, route.Gw, err)
				}
			}
		} else {
			nodes, err := n.nodeLister.List(labels.Everything())
			if err != nil {
				return err
			}
			for _, node := range nodes {
				if subnet, _ := houtil.ParseHybridOverlayHostSubnet(node); subnet != nil {
					route := makeRoute(subnet, n.drIP, mgmtPortLink)
					err := util.GetNetLinkOps().RouteDel(route)
					if err != nil && !os.IsExist(err) {
						return fmt.Errorf("failed to delete route for subnet %s via gateway %s: %v",
							route.Dst, route.Gw, err)
					}
				}
			}
		}
	}
	// Add a route via the hybrid overlay port IP through the management port
	// interface for each hybrid overlay cluster subnet
	if len(config.HybridOverlay.ClusterSubnets) > 0 {
		for _, clusterEntry := range config.HybridOverlay.ClusterSubnets {
			route := makeRoute(clusterEntry.CIDR, n.drIP, mgmtPortLink)
			err := util.GetNetLinkOps().RouteAdd(route)
			if err != nil && !os.IsExist(err) {
				return fmt.Errorf("failed to add route for subnet %s via gateway %s: %v",
					route.Dst, route.Gw, err)
			}
		}
	} else {
		nodes, err := n.nodeLister.List(labels.Everything())
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if subnet, _ := houtil.ParseHybridOverlayHostSubnet(node); subnet != nil {
				route := makeRoute(subnet, n.drIP, mgmtPortLink)
				err := util.GetNetLinkOps().RouteAdd(route)
				if err != nil && !os.IsExist(err) {
					return fmt.Errorf("failed to add route for subnet %s via gateway %s: %v",
						route.Dst, route.Gw, err)
				}
			}
		}
	}
	return nil
}

// handleHybridOverlayMACIPChange make the required changes if the nodes HybridOverlayDRIP or HybridOverlayMAC changes
func (n *NodeController) handleHybridOverlayMACIPChange(node *kapi.Node) error {
	var oldDRIP net.IP
	var oldMAC net.HardwareAddr

	newDRIPStr := node.Annotations[hotypes.HybridOverlayDRIP]
	newDRMACStr := node.Annotations[hotypes.HybridOverlayDRMAC]

	if newDRIPStr != n.drIP.String() {
		oldDRIP = n.drIP
		newDRIP := net.ParseIP(node.Annotations[hotypes.HybridOverlayDRIP])
		if newDRIP == nil {
			return fmt.Errorf("updated Hybrid Overlay Dristributed router IP annotation not a valid IP address %s", node.Annotations[hotypes.HybridOverlayDRIP])
		}
		n.drIP = newDRIP
		mgmtPortLink, err := util.GetNetLinkOps().LinkByName(types.K8sMgmtIntfName)
		if err != nil {
			return fmt.Errorf("failed to lookup link %s: %v", types.K8sMgmtIntfName, err)
		}
		err = n.createOrReplaceRoutes(mgmtPortLink, oldDRIP)
		if err != nil {
			return err
		}
	}

	if newDRMACStr != n.drMAC.String() {
		newPortMAC, err := net.ParseMAC(newDRMACStr)
		if err != nil {
			return fmt.Errorf("updated Hybrid Overlay Distributed MAC address annotation not valid %s: %v", node.Annotations[hotypes.HybridOverlayDRMAC], err)
		}
		oldMAC = n.drMAC
		n.drMAC = newPortMAC
	}

	n.recalculateFlowCache(oldDRIP, oldMAC)
	n.requestFlowSync()
	return nil
}

// EnsureHybridOverlayBridge sets up the hybrid overlay bridge
func (n *NodeController) EnsureHybridOverlayBridge(node *kapi.Node) error {
	if atomic.LoadUint32(n.initState) >= hotypes.DistributedRouterInitialized {
		if node.Annotations[hotypes.HybridOverlayDRIP] != n.drIP.String() ||
			node.Annotations[hotypes.HybridOverlayDRMAC] != n.drMAC.String() {
			if err := n.handleHybridOverlayMACIPChange(node); err != nil {
				return err
			}
		}
		return nil
	}
	if n.gwLRPIP == nil {
		gwLRPIP, err := util.ParseNodeGatewayRouterLRPAddr(node)
		if err != nil {
			return fmt.Errorf("invalid Gateway Router LRP IP: %v", err)
		}
		n.gwLRPIP = gwLRPIP
	}

	subnet, err := getLocalNodeSubnet(n.nodeName)
	if err != nil {
		return err
	}

	portName := util.GetHybridOverlayPortName(n.nodeName)
	portMACString, haveDRMACAnnotation := node.Annotations[hotypes.HybridOverlayDRMAC]
	if !haveDRMACAnnotation {
		klog.Infof("Node %s does not have DRMAC annotation yet, failed to ensure hybrid overlay"+
			"and will retry later", n.nodeName)
		// node must not be annotated yet, retry later
		return nil
	}

	portMAC, err := net.ParseMAC(portMACString)
	if err != nil {
		return fmt.Errorf("failed to parse DRMAC: %s", portMACString)
	}
	n.drMAC = portMAC

	hybridOverlayDRIP, ok := node.Annotations[hotypes.HybridOverlayDRIP]
	if !ok {
		return fmt.Errorf("hybrid overlay not initialized on %s, it was not assigned an interface address", node.Name)
	}
	n.drIP = net.ParseIP(hybridOverlayDRIP)
	if n.drIP == nil {
		return fmt.Errorf("hybrid overlay not initialized on %s, the the annotation %s = %s is not an IP address", node.Name, hotypes.HybridOverlayDRIP, hybridOverlayDRIP)
	}

	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br", extBridgeName,
		"--", "set", "Bridge", extBridgeName, "fail_mode=secure",
		"--", "set", "Interface", extBridgeName, "mtu_request="+fmt.Sprintf("%d", config.Default.MTU))
	if err != nil {
		return fmt.Errorf("failed to create hybrid-overlay bridge %s"+
			", stderr:%s: %v", extBridgeName, stderr, err)
	}

	// A OVS bridge's mac address can change when ports are added to it.
	// We cannot let that happen, so make the bridge mac address permanent.
	macAddress, err := util.GetOVSPortMACAddress(extBridgeName)
	if err != nil {
		return err
	}
	stdout, stderr, err := util.RunOVSVsctl("set", "bridge", extBridgeName, "other-config:hwaddr="+macAddress.String())
	if err != nil {
		return fmt.Errorf("failed to set bridge, stdout: %q, stderr: %q, "+
			"error: %v", stdout, stderr, err)
	}

	if _, err := util.LinkSetUp(extBridgeName); err != nil {
		return fmt.Errorf("failed to up %s: %v", extBridgeName, err)
	}

	const (
		rampInt string = "int"
		rampExt string = "ext"
	)
	// Create the connection between OVN's br-int and our hybrid overlay bridge br-ext
	_, stderr, err = util.RunOVSVsctl("--may-exist", "add-port", "br-int", rampInt,
		"--", "--may-exist", "add-port", extBridgeName, rampExt,
		"--", "set", "Interface", rampInt, "type=patch", "options:peer="+rampExt, "external-ids:iface-id="+portName,
		"--", "set", "Interface", rampExt, "type=patch", "options:peer="+rampInt)
	if err != nil {
		return fmt.Errorf("failed to create hybrid overlay bridge patch ports"+
			", stderr:%s (%v)", stderr, err)
	}

	// Add the VXLAN port for sending/receiving traffic from hybrid overlay nodes
	_, stderr, err = util.RunOVSVsctl("--may-exist", "add-port", extBridgeName, extVXLANName,
		"--", "set", "interface", extVXLANName, "type=vxlan", `options:remote_ip="flow"`, `options:key="flow"`, fmt.Sprintf("options:dst_port=%d", n.vxlanPort))
	if err != nil {
		return fmt.Errorf("failed to add VXLAN port for ovs bridge %s"+
			", stderr:%s: %v", extBridgeName, stderr, err)
	}

	flows := make([]string, 0, 10)
	// Add default drop rule to tables for easier debugging via packet counters
	for _, table := range []int{0, 1, 2, 10, 20} {
		flows = append(flows, fmt.Sprintf("table=%d,priority=0,actions=drop", table))
	}
	// Handle ARP for gateway address internally towards pods
	// resubmit to table 1 for gateway mode arp processing
	portMACRaw := strings.Replace(n.drMAC.String(), ":", "", -1)
	portIPRaw := getIPAsHexString(n.drIP)
	flows = append(flows,
		fmt.Sprintf("table=0,priority=100,in_port=%s,arp_op=1,arp,arp_tpa=%s,"+
			"actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],"+
			"mod_dl_src:%s,"+
			"load:0x2->NXM_OF_ARP_OP[],"+
			"move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],"+
			"move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],"+
			"load:0x%s->NXM_NX_ARP_SHA[],"+
			"load:0x%s->NXM_OF_ARP_SPA[],"+
			"IN_PORT,resubmit(,1)",
			rampExt, n.drIP.String(), n.drMAC.String(), portMACRaw, portIPRaw))

	// Send incoming VXLAN traffic to the pod dispatch table
	flows = append(flows,
		fmt.Sprintf("table=0,priority=100,in_port="+extVXLANName+",ip,nw_dst=%s,dl_dst=%s,actions=goto_table:10",
			subnet.String(), n.drMAC.String()))

	// Handle ARP requests from hybrid external gateway
	// First flow is low priority flow to get to table 2 (arp response table)
	// exgw will have flows that match for arp to build learn table 20, they need to be hit and then punt
	// to table 2
	// Therefore install a default low priority flow in case those flows are not installed via pod update
	flows = append(flows,
		fmt.Sprintf("table=0,priority=10,arp,in_port=%s,arp_op=1,arp_tpa=%s,"+
			"actions=resubmit(,2)",
			extVXLANName, subnet.String()))

	// Install flow to handle the arp response from exgws
	flows = append(flows,
		fmt.Sprintf("table=2,priority=100,arp,in_port=%s,arp_op=1,arp_tpa=%s,"+
			"actions=move:tun_src->tun_dst,"+
			"load:%d->NXM_NX_TUN_ID[0..31],"+
			"move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],"+
			"mod_dl_src:%s,"+
			"load:0x2->NXM_OF_ARP_OP[],"+
			"move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],"+
			"load:0x%s->NXM_NX_ARP_SHA[],"+
			"move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],"+
			"move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],"+
			"move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],"+
			"IN_PORT",
			extVXLANName, subnet.String(), hotypes.HybridOverlayVNI, n.drMAC.String(), portMACRaw))

	mgmtPortLink, err := util.GetNetLinkOps().LinkByName(types.K8sMgmtIntfName)
	if err != nil {
		return fmt.Errorf("failed to lookup link %s: %v", types.K8sMgmtIntfName, err)
	}

	err = n.createOrReplaceRoutes(mgmtPortLink, nil)
	if err != nil {
		return err
	}
	mgmtPortMAC := mgmtPortLink.Attrs().HardwareAddr

	// Add a rule to fix up return host-network traffic
	mgmtIfAddr := util.GetNodeManagementIfAddr(subnet)
	flows = append(flows,
		fmt.Sprintf("table=10,priority=100,ip,nw_dst=%s,"+
			"actions=mod_dl_src:%s,mod_dl_dst:%s,output:ext",
			mgmtIfAddr.IP.String(), portMAC.String(), mgmtPortMAC.String()))
	// Add a rule to fix up return nodePort service traffic
	gwIfAddr := util.GetNodeGatewayIfAddr(subnet)
	gwPortMAC := util.IPAddrToHWAddr(gwIfAddr.IP)
	flows = append(flows,
		fmt.Sprintf("table=10,priority=100,ip,nw_dst=%s,"+
			"actions=mod_nw_dst:%s,mod_dl_src:%s,mod_dl_dst:%s,output:ext",
			n.drIP, n.gwLRPIP.String(), portMAC.String(), gwPortMAC.String()))

	n.updateFlowCacheEntry("0x0", flows, false)
	n.requestFlowSync()
	atomic.StoreUint32(n.initState, hotypes.DistributedRouterInitialized)
	klog.Infof("Hybrid overlay setup complete for node %s", node.Name)
	return nil
}

// RunFlowSync runs flow synchronization
// It runs once when the controller is started.
// It will block until the stopCh is closed, running the sync periodically,
// or when signalled via the flowChan
func (n *NodeController) RunFlowSync(stopCh <-chan struct{}) {
	klog.Info("Starting hybrid overlay OpenFlow sync thread")
	klog.Info("Running initial OpenFlow sync")
	n.syncFlows()
	timer := time.NewTicker(n.flowCacheSyncPeriod)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			n.syncFlows()
		case <-n.flowChan:
			n.syncFlows()
			timer.Reset(n.flowCacheSyncPeriod)
		case <-stopCh:
			klog.Info("Shutting down OpenFlow sync thread")
			return
		}
	}
}

func (n *NodeController) syncFlows() {
	n.flowMutex.Lock()
	defer n.flowMutex.Unlock()
	// any learned flows in table 20 we need to store for the update, as long as they correspond to a
	// current pod in the cache
	stdout, stderr, err := util.RunOVSOfctl("dump-flows", "--no-stats", extBridgeName, "table=20")
	if err != nil {
		klog.Errorf("Failed to dump flows for flow sync, stderr: %q, error: %v", stderr, err)
		return
	}
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		// Ignore the end-of-table drop rule
		if strings.Contains(line, "actions=drop") {
			continue
		}
		line = strings.TrimSpace(line)
		cookie := strings.TrimPrefix(strings.Split(line, ",")[0], "cookie=0x")
		// the cookie from OVS will remove leading zeros, and we know the cookie length for learned flow (IP to hex)
		// is always 8, so pack with extra 0s
		for len(cookie) < 8 {
			cookie = "0" + cookie
		}
		if cacheEntry, ok := n.flowCache[cookie]; ok {
			// we ignore certain cookies for learning to avoid a case where a NS was updated with a new vtep
			// and we accidentally pick up the old vtep flow and cache it. This should only ever happen on a pod update
			// with an NS annotation VTEP change. We only need to ignore it for one iteration of sync.
			if cacheEntry.ignoreLearn {
				klog.V(5).Infof("Ignoring learned flow to add to hybrid cache for this iteration: %s", line)
				cacheEntry.ignoreLearn = false
				cacheEntry.learnedFlow = ""
				continue
			}
			// we only ever have one learned flow per pod IP
			if cacheEntry.learnedFlow != line {
				cacheEntry.learnedFlow = line
				klog.Infof("Learned flow added to hybrid flow cache: %s", line)
			}
		} else {
			klog.Warningf("Learned flow found with no matching cache entry: %s", line)
		}
	}

	flows := make([]string, 0, 100)
	for _, entry := range n.flowCache {
		flows = append(flows, entry.flows...)
		if len(entry.learnedFlow) > 0 {
			flows = append(flows, entry.learnedFlow)
		}
	}
	_, stderr, err = util.ReplaceOFFlows(extBridgeName, flows)
	if err != nil {
		klog.Errorf("Failed to add flows, error: %v, stderr: %s, flows: %s", err, stderr, flows)
	}
}

func (n *NodeController) requestFlowSync() {
	select {
	case n.flowChan <- struct{}{}:
		klog.V(5).Infof("Flow sync requested")
	default:
		klog.V(5).Infof("Sync already requested for flows")
	}
}

func (n *NodeController) updateFlowCacheEntry(cookie string, flows []string, ignoreLearn bool) {
	n.flowMutex.Lock()
	defer n.flowMutex.Unlock()
	n.flowCache[cookie] = &flowCacheEntry{flows: flows}
	n.flowCache[cookie].ignoreLearn = ignoreLearn
}

func makeRoute(dstSubnet *net.IPNet, drIP net.IP, mgmtPortLink netlink.Link) *netlink.Route {
	return &netlink.Route{
		Dst:       dstSubnet,
		LinkIndex: mgmtPortLink.Attrs().Index,
		Scope:     netlink.SCOPE_UNIVERSE,
		Gw:        drIP,
	}
}
