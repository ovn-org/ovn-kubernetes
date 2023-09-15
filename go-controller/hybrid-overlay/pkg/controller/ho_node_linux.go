package controller

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"
	kapi "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// HONodeController is the node hybrid overlay controller
// This controller will be running on hybrid overlay node. It is responsible for
//  1. Add Hybrid Overlay DRMAC annotation to its own node object,
//  2. Remove the ovnkube pod annotations from the pods running on this node.
type HONodeController struct {
	kube        kube.Interface
	nodeName    string
	localNodeIP net.IP
}

// newHONodeController returns a node handler that listens for node events
// so that Add/Update/Delete events are appropriately handled.
func newHONodeController(kube kube.Interface,
	nodeName string,
	nodeLister listers.NodeLister,
	podLister listers.PodLister,
) (nodeController, error) {
	return &HONodeController{
		kube:     kube,
		nodeName: nodeName,
	}, nil
}

// AddNode set annotations about its VTEP and gateway MAC address to its own node object
func (n *HONodeController) AddNode(node *kapi.Node) error {
	if node.Name != n.nodeName {
		return nil
	}

	cidr, nodeIP := getNodeSubnetAndIP(node)
	if cidr == nil {
		return fmt.Errorf("failed to get hybrid overlay subnet the local node")
	}

	if nodeIP == nil {
		return fmt.Errorf("failed to get nodeIP of the local node")
	}

	if nodeIP.Equal(n.localNodeIP) {
		// Node IP doesn't changed. Skip updating hybrid overlay DRMAC annotation
		return nil
	}
	n.localNodeIP = nodeIP

	drMAC, err := getHostInterfaceMAC(n.localNodeIP)
	if err != nil {
		return err
	}

	if drMAC == "" {
		return fmt.Errorf("cannot to find the hybrid overlay distributed router gateway MAC address")
	}

	klog.Infof("Set hybrid overlay DRMAC annotation: %s", drMAC)
	if err := n.kube.SetAnnotationsOnNode(node.Name, map[string]interface{}{
		types.HybridOverlayDRMAC: drMAC,
	}); err != nil {
		return fmt.Errorf("failed to set DRMAC annotation on node: %v", err)
	}
	return nil
}

// Delete handles node deletions
func (n *HONodeController) DeleteNode(node *kapi.Node) error {
	return nil
}

// AddPod remove the ovnkube annotation from the pods running on its own node
func (n *HONodeController) AddPod(pod *kapi.Pod) error {
	if pod.Spec.NodeName != n.nodeName {
		return nil
	}

	_, ok := pod.Annotations[util.OvnPodAnnotationName]
	if ok {
		klog.Infof("Remove the ovnkube pod annotation from pod %s", pod.Name)
		delete(pod.Annotations, util.OvnPodAnnotationName)
		if err := n.kube.UpdatePodStatus(pod); err != nil {
			return fmt.Errorf("failed to remove ovnkube pod annotation from pod %s: %v", pod.Name, err)
		}
		return nil
	}
	return nil
}

func (n *HONodeController) DeletePod(pod *kapi.Pod) error {
	return nil
}

func (n *HONodeController) RunFlowSync(stopCh <-chan struct{}) {}

func (n *HONodeController) EnsureHybridOverlayBridge(node *kapi.Node) error {
	return nil
}

func getHostInterfaceMAC(ip net.IP) (string, error) {
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return "", err
	}
	for _, link := range links {
		addrs, err := util.GetNetLinkOps().AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return "", fmt.Errorf("failed to get IP address for link %s: %v", link.Attrs().Name, err)
		}
		for _, add := range addrs {
			if add.IP.Equal(ip) {
				return link.Attrs().HardwareAddr.String(), nil
			}
		}
	}
	return "", fmt.Errorf("failed to get IP address for node IP: %s", ip)
}
