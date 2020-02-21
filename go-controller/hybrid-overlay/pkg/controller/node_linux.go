package controller

import (
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/sirupsen/logrus"

	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	extBridgeName string = "br-ext"
)

// NodeController is the node hybrid overlay controller
type NodeController struct {
	kube     *kube.Kube
	subnet   *net.IPNet
	nodeName string
	drMAC    string
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
		logrus.Debugf("cleaning up hybrid overlay pod %s/%s because %v", pod.Namespace, pod.Name, err)
		return n.deletePod(pod)
	}

	cookie := podToCookie(pod)
	_, _, err = util.RunOVSOfctl("add-flow", extBridgeName,
		fmt.Sprintf("table=10, cookie=0x%s, priority=100, ip, nw_dst=%s, actions=set_field:%s->eth_src,set_field:%s->eth_dst,output:ext", cookie, podIP, n.drMAC, podMAC))
	if err != nil {
		return fmt.Errorf("failed to add flows for pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	return nil
}

func (n *NodeController) deletePod(pod *kapi.Pod) error {
	if pod.Spec.NodeName != n.nodeName {
		return nil
	}

	cookie := podToCookie(pod)
	_, _, err := util.RunOVSOfctl("del-flows", extBridgeName, fmt.Sprintf("table=10, cookie=0x%s/0xffffffff", cookie))
	if err != nil {
		return fmt.Errorf("failed to delete flows for pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	return nil
}

func getPodDetails(pod *kapi.Pod, nodeName string) (string, string, error) {
	if pod.Spec.NodeName != nodeName {
		return "", "", fmt.Errorf("not scheduled")
	}

	podInfo, err := util.UnmarshalPodAnnotation(pod.Annotations)
	if err != nil {
		return "", "", err
	}
	return podInfo.IP.String(), podInfo.MAC.String(), nil
}

// podChanged returns true if any relevant pod attributes changed
func podChanged(pod1 *kapi.Pod, pod2 *kapi.Pod, nodeName string) bool {
	podIP1, mac1, _ := getPodDetails(pod1, nodeName)
	podIP2, mac2, _ := getPodDetails(pod2, nodeName)
	return (podIP1 != podIP2 || mac1 != mac2)
}

func (n *NodeController) syncPods(pods []interface{}) {
	kubePods := make(map[string]bool)
	for _, tmp := range pods {
		pod, ok := tmp.(*kapi.Pod)
		if !ok {
			logrus.Errorf("Spurious object in syncPods: %v", tmp)
			continue
		}
		kubePods[podToCookie(pod)] = true
	}

	stdout, stderr, err := util.RunOVSOfctl("dump-flows", extBridgeName, "table=10")
	if err != nil {
		logrus.Errorf("failed to dump flows for %s: stderr: %q, error: %v", extBridgeName, stderr, err)
		return
	}

	// Find all flows that exist in br-ext that are for pods not present
	// in the Kube pod list
	lines := strings.Split(stdout, "\n")
	podsToRemove := make(map[string]bool)
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
				podsToRemove[cookie] = true
			}
		}
	}

	for cookie := range podsToRemove {
		stderr, _, err = util.RunOVSOfctl("del-flows", extBridgeName, fmt.Sprintf("table=10, cookie=0x%s/0xffffffff", cookie))
		if err != nil {
			logrus.Errorf("Failed clean stale hybrid overlay pod flow %q: stderr: %q, error: %v",
				cookie, stderr, err)
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
				logrus.Warningf("failed to handle pod %v addition: %v", pod, err)
			}
		},
		UpdateFunc: func(old, newer interface{}) {
			podNew := newer.(*kapi.Pod)
			podOld := old.(*kapi.Pod)
			if podChanged(podOld, podNew, n.nodeName) {
				if err := n.addOrUpdatePod(podNew); err != nil {
					logrus.Warningf("failed to handle pod %v update: %v", podNew, err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			if err := n.deletePod(pod); err != nil {
				logrus.Warningf("failed to handle pod %v deletion: %v", pod, err)
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

// windowsNodeAddOrUpdate sets up or tears down VXLAN tunnels to Windows nodes in the
// cluster
func (n *NodeController) windowsNodeAddOrUpdate(node *kapi.Node) error {
	if !houtil.IsWindowsNode(node) {
		return nil
	}

	cidr, nodeIP, drMAC := getNodeDetails(node, true)
	if cidr == nil || nodeIP == nil || drMAC == nil {
		n.Delete(node)
		return nil
	}

	// (re)add flows for the node
	cookie := nameToCookie(node.Name)
	drMACRaw := strings.Replace(drMAC.String(), ":", "", -1)

	// Distributed Router MAC ARP responder flow; responds to ARP requests by OVN for
	// any IP address within this node's assigned subnet and returns our hybrid overlay
	// port's MAC address.
	_, _, err := util.RunOVSOfctl("add-flow", extBridgeName,
		fmt.Sprintf("cookie=0x%s,table=0,priority=100,arp,in_port=ext,arp_tpa=%s actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:%s,load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],load:0x%s->NXM_NX_ARP_SHA[],move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],IN_PORT", cookie, cidr.String(), drMAC.String(), drMACRaw))
	if err != nil {
		return fmt.Errorf("failed to add ARP responder flow for node %q: %v", node.Name, err)
	}

	// Send all flows for the remote node's assigned subnet to that node via the VXLAN tunnel.
	// Windows implementation requires that we set the destination MAC address to the node's
	// Distributed Router MAC.
	_, _, err = util.RunOVSOfctl("add-flow", extBridgeName,
		fmt.Sprintf("cookie=0x%s,table=0,priority=100,ip,nw_dst=%s,actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:%s->tun_dst,set_field:%s->eth_dst,output:1", cookie, cidr.String(), nodeIP.String(), drMAC.String()))
	if err != nil {
		return fmt.Errorf("failed to add VXLAN flow for node %q: %v", node.Name, err)
	}

	return nil
}

// Add handles node additions
func (n *NodeController) Add(node *kapi.Node) {
	if node.Name != n.nodeName {
		if err := n.windowsNodeAddOrUpdate(node); err != nil {
			logrus.Warning(err)
		}
	}
}

// Update handles node updates
func (n *NodeController) Update(oldNode, newNode *kapi.Node) {
	if newNode.Name != n.nodeName {
		if nodeChanged(oldNode, newNode) {
			if err := n.windowsNodeAddOrUpdate(newNode); err != nil {
				logrus.Warning(err)
			}
		}
	}
}

func deleteNodeFlowsByCookie(cookie string) error {
	_, _, err := util.RunOVSOfctl("del-flows", extBridgeName, fmt.Sprintf("table=0, cookie=0x%s/0xffffffff", cookie))
	if err != nil {
		return fmt.Errorf("failed to delete flows for node cookie %q: %v", cookie, err)
	}
	return nil
}

// Delete handles node deletions
func (n *NodeController) Delete(node *kapi.Node) {
	if node.Name == n.nodeName || !houtil.IsWindowsNode(node) {
		return
	}

	if err := deleteNodeFlowsByCookie(nameToCookie(node.Name)); err != nil {
		logrus.Errorf(err.Error())
	}
}

// Sync handles local node initialization and removing stale nodes on startup
func (n *NodeController) Sync(nodes []*kapi.Node) {
	kubeNodes := make(map[string]bool)
	for _, node := range nodes {
		if houtil.IsWindowsNode(node) {
			kubeNodes[nameToCookie(node.Name)] = true
		}
	}

	stdout, stderr, err := util.RunOVSOfctl("dump-flows", extBridgeName, "table=0")
	if err != nil {
		logrus.Errorf("failed to dump flows for %s: stderr: %q, error: %v", extBridgeName, stderr, err)
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
			if _, ok := kubeNodes[cookie]; !ok {
				nodesToRemove[cookie] = true
			}
		}
	}

	for cookie := range nodesToRemove {
		if err := deleteNodeFlowsByCookie(cookie); err != nil {
			logrus.Errorf("Failed clean stale hybrid overlay node flow %q: %v", cookie, err)
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

	logrus.Infof("found node %s subnet %s", nodeName, subnet.String())
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
	var err error
	if n.subnet, err = getLocalNodeSubnet(n.nodeName); err != nil {
		return err
	}

	portName := houtil.GetHybridOverlayPortName(n.nodeName)

	// If the master hasn't yet created our hybrid overlay port, try again later
	portMAC, portIP, _ := util.GetPortAddresses(portName)
	if portMAC == nil || portIP == nil {
		return nil
	}

	// if our bridge already exists, nothing to do
	if _, _, err := util.RunOVSVsctl("br-exists", extBridgeName); err == nil {
		return nil
	}

	// Otherwise set things up
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
		portNum string = "11"
	)
	// Create the connection between OVN's br-int and our hybrid overlay bridge br-ext
	_, stderr, err = util.RunOVSVsctl("--may-exist", "add-port", "br-int", rampInt,
		"--", "--may-exist", "add-port", extBridgeName, rampExt,
		"--", "set", "Interface", rampInt, "type=patch", "options:peer="+rampExt, "external-ids:iface-id="+portName,
		"--", "set", "Interface", rampExt, "type=patch", "options:peer="+rampInt, "ofport_request="+portNum)
	if err != nil {
		return fmt.Errorf("Failed to create hybrid overlay bridge patch ports"+
			", stderr:%s (%v)", stderr, err)
	}

	// Add default drop rule to table 0 for easier debugging via packet counters
	_, stderr, err = util.RunOVSOfctl("-O", "openflow13", "add-flow", extBridgeName, "table=0, priority=0, actions=drop")
	if err != nil {
		return fmt.Errorf("failed to set up hybrid overlay bridge default drop rule,"+
			"stderr: %q, error: %v", stderr, err)
	}

	// Handle ARP for gateway address internally
	portMACRaw := strings.Replace(portMAC.String(), ":", "", -1)
	portIPRaw := getIPAsHexString(portIP)
	_, stderr, err = util.RunOVSOfctl("-O", "openflow13", "add-flow", extBridgeName,
		"table=0, priority=100, in_port="+portNum+", arp, arp_tpa="+portIP.String()+", actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:"+portMAC.String()+",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],load:0x"+portMACRaw+"->NXM_NX_ARP_SHA[],load:0x"+portIPRaw+"->NXM_OF_ARP_SPA[],IN_PORT")
	if err != nil {
		return fmt.Errorf("failed to set up hybrid overlay bridge ARP flow,"+
			"stderr: %q, error: %v", stderr, err)
	}

	// Add the VXLAN port for sending/receiving traffic from Windows nodes
	_, stderr, err = util.RunOVSVsctl("--may-exist", "add-port", extBridgeName, "ext-vxlan",
		"--", "set", "interface", "ext-vxlan", "ofport_request=1", "type=vxlan", `options:remote_ip="flow"`, `options:key="flow"`)
	if err != nil {
		return fmt.Errorf("Failed to add VXLAN port for ovs bridge %s"+
			", stderr:%s: %v", extBridgeName, stderr, err)
	}

	// Send incoming VXLAN traffic to the pod dispatch table
	_, stderr, err = util.RunOVSOfctl("-O", "openflow13", "add-flow", extBridgeName,
		fmt.Sprintf("table=0, priority=100, in_port=ext-vxlan, ip, nw_dst=%s, dl_dst=%s, actions=goto_table:10", n.subnet.String(), portMAC.String()))
	if err != nil {
		return fmt.Errorf("failed to set up hybrid overlay bridge ARP flow,"+
			"stderr: %q, error: %v", stderr, err)
	}

	// Default drop rule for incoming VXLAN traffic that matches no running pod
	_, stderr, err = util.RunOVSOfctl("-O", "openflow13", "add-flow", extBridgeName, "table=10, priority=0, actions=drop")
	if err != nil {
		return fmt.Errorf("failed to set up hybrid overlay bridge pod dispatch default drop rule,"+
			"stderr: %q, error: %v", stderr, err)
	}

	// Allow VXLAN traffic on the host
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return err
	}
	rule := []string{"-p", "udp", "-m", "udp", "--dport", "4789", "-j", "ACCEPT"}
	if exists, _ := ipt.Exists("filter", "INPUT", rule...); !exists {
		if err = ipt.Insert("filter", "INPUT", 1, rule...); err != nil {
			return fmt.Errorf("could not set up iptables rules for hybrid overlay VXLAN: %v", err)
		}
	}
	n.drMAC = portMAC.String()

	return nil
}
