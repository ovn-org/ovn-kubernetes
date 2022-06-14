package controller

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// NodeController is the node hybrid overlay controller
type NodeController struct {
	kube            kube.Interface
	machineID       string
	nodeName        string
	localNodeCIDR   *net.IPNet
	localNodeIP     net.IP
	remoteSubnetMap map[string]string // Maps a remote node to its remote subnet
}

// newNodeController returns a node handler that listens for node events
// so that Add/Update/Delete events are appropriately handled.
func newNodeController(kube kube.Interface,
	nodeName string,
	nodeLister listers.NodeLister,
) (nodeController, error) {

	node, err := kube.GetNode(nodeName)
	if err != nil {
		return nil, err
	}

	return &NodeController{
		kube:            kube,
		machineID:       node.Status.NodeInfo.MachineID,
		nodeName:        node.Name,
		remoteSubnetMap: make(map[string]string),
	}, nil
}

// AddNode set annotations about its VTEP and gateway MAC address to its own node object
func (n *NodeController) AddNode(node *kapi.Node) error {
	if node.Name == n.nodeName {
		cidr, nodeIP := getNodeSubnetAndIP(node)
		if (cidr != nil && !houtil.SameIPNet(cidr, n.localNodeCIDR)) || (nodeIP != nil && nodeIP.Equal(n.localNodeIP)) {
			n.localNodeCIDR = cidr
			n.localNodeIP = nodeIP

			var drMAC string
			interfaces, _ := net.Interfaces()
			ip := n.localNodeIP.To4().String()
		out:
			for _, interf := range interfaces {
				if addrs, err := interf.Addrs(); err == nil {
					for _, addr := range addrs {
						if strings.Contains(addr.String(), ip) {
							netInterface, err := net.InterfaceByName(interf.Name)
							if err != nil {
								return fmt.Errorf("failed to get interface by name: %s: %v", interf.Name, err)
							}
							drMAC = netInterface.HardwareAddr.String()
							break out
						}
					}
				}
			}
			klog.Info("Set hybrid overlay DRMAC annotation")
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
func (n *NodeController) DeleteNode(node *kapi.Node) error {
	return nil
}

// AddPod remove the ovnkube annotation from the pods running on its own node
func (n *NodeController) AddPod(pod *kapi.Pod) error {
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

func (n *NodeController) DeletePod(pod *kapi.Pod) error {
	return nil
}

func (n *NodeController) RunFlowSync(stopCh <-chan struct{}) {}

func (n *NodeController) EnsureHybridOverlayBridge(node *kapi.Node) error {
	return nil
}
