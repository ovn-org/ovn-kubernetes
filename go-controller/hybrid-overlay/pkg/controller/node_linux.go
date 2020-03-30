package controller

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
	"time"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

const (
	extBridgeName string = "br-ext"
	extVXLANName  string = "ext-vxlan"
)

// NodeController is the node hybrid overlay controller
type NodeController struct {
	kube        *kube.Kube
	nodeName    string
	initialized bool
	drMAC       string
}

// NewNode returns a node handler that listens for node events
// so that Add/Update/Delete events are appropriately handled.
// It initializes the node it is currently running on. On Linux, this means:
//  1. Setting up a VXLAN gateway and hooking to the OVN gateway
//  2. Setting back annotations about its VTEP and gateway MAC address to its own object
func NewNode(clientset kubernetes.Interface, nodeName string) (*NodeController, error) {
	node := &NodeController{
		kube:     &kube.Kube{KClient: clientset},
		nodeName: nodeName,
	}
	if err := node.ensureHybridOverlayBridge(); err != nil {
		return nil, err
	}
	return node, nil
}

func podToCookie(pod *kapi.Pod) string {
	return nameToCookie(pod.Namespace + "_" + pod.Name)
}

func (n *NodeController) addOrUpdatePod(pod *kapi.Pod) error {
	podIP, podMAC, err := getPodDetails(pod, n.nodeName)
	if err != nil {
		klog.V(5).Infof("cleaning up hybrid overlay pod %s/%s because %v", pod.Namespace, pod.Name, err)
		return n.deletePod(pod)
	}

	cookie := podToCookie(pod)
	_, _, err = util.RunOVSOfctl("add-flow", extBridgeName,
		fmt.Sprintf("table=10,cookie=0x%s,priority=100,ip,nw_dst=%s,actions=set_field:%s->eth_src,set_field:%s->eth_dst,output:ext", cookie, podIP.IP, n.drMAC, podMAC))
	if err != nil {
		return fmt.Errorf("failed to add flows for pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	return nil
}

func (n *NodeController) deletePod(pod *kapi.Pod) error {
	if pod.Spec.NodeName == n.nodeName {
		if err := deleteFlowsByCookie(10, podToCookie(pod)); err != nil {
			return fmt.Errorf("failed to delete flows for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

func getPodDetails(pod *kapi.Pod, nodeName string) (*net.IPNet, net.HardwareAddr, error) {
	if pod.Spec.NodeName != nodeName {
		return nil, nil, fmt.Errorf("not scheduled")
	}

	podInfo, err := util.UnmarshalPodAnnotation(pod.Annotations)
	if err != nil {
		return nil, nil, err
	}
	return podInfo.IP, podInfo.MAC, nil
}

// podChanged returns true if any relevant pod attributes changed
func podChanged(pod1 *kapi.Pod, pod2 *kapi.Pod, nodeName string) bool {
	podIP1, mac1, _ := getPodDetails(pod1, nodeName)
	podIP2, mac2, _ := getPodDetails(pod2, nodeName)
	return podIP1.String() != podIP2.String() || !bytes.Equal(mac1, mac2)
}

func (n *NodeController) syncPods(pods []interface{}) {
	kubePods := make(map[string]bool)
	for _, tmp := range pods {
		pod, ok := tmp.(*kapi.Pod)
		if !ok {
			klog.Errorf("Spurious object in syncPods: %v", tmp)
			continue
		}
		kubePods[podToCookie(pod)] = true
	}

	stdout, stderr, err := util.RunOVSOfctl("dump-flows", extBridgeName, "table=10")
	if err != nil {
		klog.Errorf("failed to dump flows for %s: stderr: %q, error: %v", extBridgeName, stderr, err)
		return
	}

	// Find all flows that exist in br-ext that are for pods not present
	// in the Kube pod list
	lines := strings.Split(stdout, "\n")
	cookiesToRemove := make(map[string]bool)
	for _, line := range lines {
		// Ignore the end-of-table drop rule
		if strings.Contains(line, "actions=drop") {
			continue
		}

		parts := strings.Split(line, ",")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			const cookieTag string = "cookie=0x"
			if !strings.HasPrefix(part, cookieTag) {
				continue
			}
			cookie := part[len(cookieTag):]
			if _, ok := kubePods[cookie]; !ok {
				cookiesToRemove[cookie] = true
			}
		}
	}

	for cookie := range cookiesToRemove {
		if err := deleteFlowsByCookie(10, cookie); err != nil {
			klog.Errorf("failed clean stale hybrid overlay pod flow %q: %v", cookie, err)
		}
	}
}

// Start is the top level function to run hybrid-sdn in node mode
func (n *NodeController) Start(wf *factory.WatchFactory) error {
	if err := n.startNodeWatch(wf); err != nil {
		return err
	}

	return n.startPodWatch(wf)
}

func (n *NodeController) startPodWatch(wf *factory.WatchFactory) error {
	_, err := wf.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			if err := n.addOrUpdatePod(pod); err != nil {
				klog.Warningf("failed to handle pod %v addition: %v", pod, err)
			}
		},
		UpdateFunc: func(old, newer interface{}) {
			podNew := newer.(*kapi.Pod)
			podOld := old.(*kapi.Pod)
			if podChanged(podOld, podNew, n.nodeName) {
				if err := n.addOrUpdatePod(podNew); err != nil {
					klog.Warningf("failed to handle pod %v update: %v", podNew, err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			if err := n.deletePod(pod); err != nil {
				klog.Warningf("failed to handle pod %v deletion: %v", pod, err)
			}
		},
	}, n.syncPods)
	return err
}

func (n *NodeController) startNodeWatch(wf *factory.WatchFactory) error {
	return houtil.StartNodeWatch(n, wf)
}

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
		klog.V(5).Infof("cleaning up hybrid overlay resources for node %q because: %v", node.Name, err)
		n.Delete(node)
		return nil
	}

	klog.Infof("setting up hybrid overlay tunnel to node %s", node.Name)

	// (re)add flows for the node
	cookie := nameToCookie(node.Name)
	drMACRaw := strings.Replace(drMAC.String(), ":", "", -1)

	// Distributed Router MAC ARP responder flow; responds to ARP requests by OVN for
	// any IP address within this node's assigned subnet and returns our hybrid overlay
	// port's MAC address.
	_, _, err = util.RunOVSOfctl("add-flow", extBridgeName,
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
	if err != nil {
		return fmt.Errorf("failed to add ARP responder flow for node %q: %v", node.Name, err)
	}

	// Send all flows for the remote node's assigned subnet to that node via the VXLAN tunnel.
	// Windows hybrid overlay implementation requires that we set the destination MAC address
	// to the node's Distributed Router MAC.
	_, _, err = util.RunOVSOfctl("add-flow", extBridgeName,
		fmt.Sprintf("cookie=0x%s,table=0,priority=100,ip,nw_dst=%s,"+
			"actions=load:%d->NXM_NX_TUN_ID[0..31],"+
			"set_field:%s->tun_dst,"+
			"set_field:%s->eth_dst,"+
			"output:"+extVXLANName,
			cookie, cidr.String(), hotypes.HybridOverlayVNI, nodeIP.String(), drMAC.String()))
	if err != nil {
		return fmt.Errorf("failed to add VXLAN flow for node %q: %v", node.Name, err)
	}

	return nil
}

// Add handles node additions and updates
func (n *NodeController) Add(node *kapi.Node) {
	var err error
	if node.Name == n.nodeName {
		// Retry hybrid overlay initialization if the master was
		// slow to add the hybrid overlay logical network elements
		err = n.ensureHybridOverlayBridge()
	} else {
		err = n.hybridOverlayNodeUpdate(node)
	}

	if err != nil {
		klog.Warning(err)
	}
}

// Update handles node updates
func (n *NodeController) Update(oldNode, newNode *kapi.Node) {
	if nodeChanged(oldNode, newNode) {
		n.Delete(newNode)
		n.Add(newNode)
	}
}

func deleteFlowsByCookie(table int, cookie string) error {
	_, stderr, err := util.RunOVSOfctl("del-flows", extBridgeName, fmt.Sprintf("table=%d,cookie=0x%s/0xffffffff", table, cookie))
	if err != nil {
		return fmt.Errorf("failed to delete table %d flows for cookie %q: %v, stderr: %v", table, cookie, err, stderr)
	}
	return nil
}

// Delete handles node deletions
func (n *NodeController) Delete(node *kapi.Node) {
	if node.Name == n.nodeName || !houtil.IsHybridOverlayNode(node) {
		return
	}

	if err := deleteFlowsByCookie(0, nameToCookie(node.Name)); err != nil {
		klog.Errorf(err.Error())
	}
}

// Sync handles local node initialization and removing stale nodes on startup
func (n *NodeController) Sync(nodes []*kapi.Node) {
	hybridOverlayNodes := make(map[string]bool)
	for _, node := range nodes {
		if houtil.IsHybridOverlayNode(node) {
			hybridOverlayNodes[nameToCookie(node.Name)] = true
		}
	}

	stdout, stderr, err := util.RunOVSOfctl("dump-flows", extBridgeName, "table=0")
	if err != nil {
		klog.Errorf("failed to dump flows for %s: stderr: %q, error: %v", extBridgeName, stderr, err)
		return
	}

	// Find all flows that exist in br-ext that are for nodes not present
	// in the Kube node list
	lines := strings.Split(stdout, "\n")
	nodesToRemove := make(map[string]bool)
	for _, line := range lines {
		// Ignore the end-of-table drop rule
		if strings.Contains(line, "actions=drop") {
			continue
		}

		parts := strings.Split(line, ",")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			const cookieTag string = "cookie=0x"
			if !strings.HasPrefix(part, cookieTag) {
				continue
			}
			cookie := part[len(cookieTag):]
			if len(cookie) != 8 {
				// Ignore non-node-specific rules (eg cookie=0x0)
				continue
			}
			if _, ok := hybridOverlayNodes[cookie]; !ok {
				nodesToRemove[cookie] = true
			}
		}
	}

	for cookie := range nodesToRemove {
		if err := deleteFlowsByCookie(0, cookie); err != nil {
			klog.Errorf("Failed clean stale hybrid overlay node flow %q: %v", cookie, err)
		}
	}
}

func getLocalNodeSubnet(nodeName string) (*net.IPNet, error) {
	var cidr string
	var err error

	// First wait for the node logical switch to be created by the Master, timeout is 300s.
	if err := wait.PollImmediate(500*time.Millisecond, 300*time.Second, func() (bool, error) {
		if cidr, _, err = util.RunOVNNbctl("get", "logical_switch", nodeName, "other-config:subnet"); err != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("timed out waiting for node %q logical switch: %v", nodeName, err)
	}

	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("Invalid hostsubnet found for node %s - %v", nodeName, err)
	}

	klog.Infof("found node %s subnet %s", nodeName, subnet.String())
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

func (n *NodeController) ensureHybridOverlayBridge() error {
	if n.initialized {
		return nil
	}

	subnet, err := getLocalNodeSubnet(n.nodeName)
	if err != nil {
		return err
	}

	portName := houtil.GetHybridOverlayPortName(n.nodeName)

	// If the master hasn't yet created our hybrid overlay port don't
	// return an error, but allow the caller to try again later
	portMAC, portIP, _ := util.GetPortAddresses(portName)
	if portMAC == nil || portIP == nil {
		return nil
	}

	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br", extBridgeName,
		"--", "set", "Bridge", extBridgeName, "fail_mode=secure")
	if err != nil {
		return fmt.Errorf("Failed to create localnet bridge %s"+
			", stderr:%s: %v", extBridgeName, stderr, err)
	}

	// A OVS bridge's mac address can change when ports are added to it.
	// We cannot let that happen, so make the bridge mac address permanent.
	macAddress, err := util.GetOVSPortMACAddress(extBridgeName)
	if err != nil {
		return err
	}
	stdout, stderr, err := util.RunOVSVsctl("set", "bridge", extBridgeName, "other-config:hwaddr="+macAddress)
	if err != nil {
		return fmt.Errorf("Failed to set bridge, stdout: %q, stderr: %q, "+
			"error: %v", stdout, stderr, err)
	}

	if _, _, err = util.RunIP("link", "set", extBridgeName, "up"); err != nil {
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
		return fmt.Errorf("Failed to create hybrid overlay bridge patch ports"+
			", stderr:%s (%v)", stderr, err)
	}

	// Add default drop rule to table 0 for easier debugging via packet counters
	_, stderr, err = util.RunOVSOfctl("add-flow", extBridgeName, "table=0,priority=0,actions=drop")
	if err != nil {
		return fmt.Errorf("failed to set up hybrid overlay bridge default drop rule,"+
			"stderr: %q, error: %v", stderr, err)
	}

	// Handle ARP for gateway address internally
	portMACRaw := strings.Replace(portMAC.String(), ":", "", -1)
	portIPRaw := getIPAsHexString(portIP)
	_, stderr, err = util.RunOVSOfctl("add-flow", extBridgeName,
		fmt.Sprintf("table=0,priority=100,in_port=%s,arp,arp_tpa=%s,"+
			"actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],"+
			"mod_dl_src:%s,"+
			"load:0x2->NXM_OF_ARP_OP[],"+
			"move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],"+
			"move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],"+
			"load:0x%s->NXM_NX_ARP_SHA[],"+
			"load:0x%s->NXM_OF_ARP_SPA[],"+
			"IN_PORT",
			rampExt, portIP.String(), portMAC.String(), portMACRaw, portIPRaw))
	if err != nil {
		return fmt.Errorf("failed to set up hybrid overlay bridge ARP flow,"+
			"stderr: %q, error: %v", stderr, err)
	}

	// Add the VXLAN port for sending/receiving traffic from hybrid overlay nodes
	_, stderr, err = util.RunOVSVsctl("--may-exist", "add-port", extBridgeName, extVXLANName,
		"--", "set", "interface", extVXLANName, "type=vxlan", `options:remote_ip="flow"`, `options:key="flow"`)
	if err != nil {
		return fmt.Errorf("Failed to add VXLAN port for ovs bridge %s"+
			", stderr:%s: %v", extBridgeName, stderr, err)
	}

	// Send incoming VXLAN traffic to the pod dispatch table
	_, stderr, err = util.RunOVSOfctl("add-flow", extBridgeName,
		fmt.Sprintf("table=0,priority=100,in_port="+extVXLANName+",ip,nw_dst=%s,dl_dst=%s,actions=goto_table:10",
			subnet.String(), portMAC.String()))
	if err != nil {
		return fmt.Errorf("failed to set up hybrid overlay bridge ARP flow,"+
			"stderr: %q, error: %v", stderr, err)
	}

	// Default drop rule for incoming VXLAN traffic that matches no running pod
	_, stderr, err = util.RunOVSOfctl("add-flow", extBridgeName, "table=10,priority=0,actions=drop")
	if err != nil {
		return fmt.Errorf("failed to set up hybrid overlay bridge pod dispatch default drop rule,"+
			"stderr: %q, error: %v", stderr, err)
	}

	n.drMAC = portMAC.String()

	n.initialized = true
	return nil
}
