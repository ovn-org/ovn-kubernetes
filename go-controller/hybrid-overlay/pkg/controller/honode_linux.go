package controller

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"
	kapi "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// HONodeController is the node hybrid overlay controller
type HONodeController struct {
	kube            kube.Interface
	machineID       string
	nodeName        string
	localNodeCIDR   *net.IPNet
	localNodeIP     net.IP
	remoteSubnetMap map[string]string // Maps a remote node to its remote subnet
}

// newHONodeController returns a node handler that listens for node events
// so that Add/Update/Delete events are appropriately handled.
func newHONodeController(kube kube.Interface,
	nodeName string,
	nodeLister listers.NodeLister,
) (nodeController, error) {

	node, err := kube.GetNode(nodeName)
	if err != nil {
		return nil, err
	}

	return &HONodeController{
		kube:            kube,
		machineID:       node.Status.NodeInfo.MachineID,
		nodeName:        node.Name,
		remoteSubnetMap: make(map[string]string),
	}, nil
}

// AddNode set annotations about its VTEP and gateway MAC address to its own node object
func (n *HONodeController) AddNode(node *kapi.Node) error {
	if node.Name == n.nodeName {
		cidr, nodeIP := getNodeSubnetAndIP(node)
		if (cidr != nil && !houtil.SameIPNet(cidr, n.localNodeCIDR)) || (nodeIP != nil && nodeIP.Equal(n.localNodeIP)) {
			n.localNodeCIDR = cidr
			n.localNodeIP = nodeIP

			var drMAC string

			links, err := util.GetNetLinkOps().LinkList()
			if err != nil {
				return err
			}
		out:
			for _, link := range links {
				addrs, err := util.GetNetLinkOps().AddrList(link, netlink.FAMILY_ALL)
				if err != nil {
					return fmt.Errorf("failed to get IP address for link %s: %v", link.Attrs().Name, err)
				}
				for _, add := range addrs {
					if add.IP.Equal(n.localNodeIP) {
						drMAC = link.Attrs().HardwareAddr.String()
						break out
					}
				}
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
		}
		return nil
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
		if err := n.kube.UpdatePod(pod); err != nil {
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
