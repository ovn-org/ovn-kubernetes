package controller

import (
	"bytes"
	"fmt"
	"net"

	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/allocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
)

// MasterController is the master hybrid overlay controller
type MasterController struct {
	kube      *kube.Kube
	allocator *allocator.SubnetAllocator
}

// NewMaster a new master controller that listens for node events
func NewMaster(clientset kubernetes.Interface, subnets []config.CIDRNetworkEntry) (*MasterController, error) {
	m := &MasterController{
		kube:      &kube.Kube{KClient: clientset},
		allocator: allocator.NewSubnetAllocator(),
	}

	// Add our hybrid overlay CIDRs to the allocator
	for _, clusterEntry := range subnets {
		err := m.allocator.AddNetworkRange(clusterEntry.CIDR.String(), 32-clusterEntry.HostSubnetLength)
		if err != nil {
			return nil, err
		}
	}

	// Mark existing hostsubnets as already allocated
	existingNodes, err := m.kube.GetNodes()
	if err != nil {
		return nil, fmt.Errorf("Error in initializing/fetching subnets: %v", err)
	}
	for _, node := range existingNodes.Items {
		if houtil.IsWindowsNode(&node) {
			hostsubnet, ok := node.Annotations[types.HybridOverlayNodeSubnet]
			if ok {
				if err := m.allocator.MarkAllocatedNetwork(hostsubnet); err != nil {
					utilruntime.HandleError(err)
				}
			}
		}
	}

	return m, nil
}

// Start is the top level function to run hybrid overlay in master mode
func (m *MasterController) Start(wf *factory.WatchFactory) error {
	return houtil.StartNodeWatch(m, wf)
}

func parseHybridOverlayNodeHostSubnet(node *kapi.Node) (*net.IPNet, error) {
	sub, ok := node.Annotations[types.HybridOverlayNodeSubnet]
	if !ok {
		return nil, nil
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return nil, fmt.Errorf("error parsing node %s annotation %s value %q: %v",
			node.Name, types.HybridOverlayNodeSubnet, sub, err)
	}

	return subnet, nil
}

func sameCIDR(a, b *net.IPNet) bool {
	if a == b {
		return true
	} else if (a == nil && b != nil) || (a != nil && b == nil) {
		return false
	}
	return a.IP.Equal(b.IP) && bytes.Equal(a.Mask, b.Mask)
}

// updateNodeAnnotation returns:
// 1) the annotation name
// 2) the annotation value (if any)
// 3) true to add the annotation, false to delete it from the node
// 4) any error that occurred
func (m *MasterController) updateNodeAnnotation(node *kapi.Node, annotator kube.Annotator) error {
	extNodeSubnet, _ := parseHybridOverlayNodeHostSubnet(node)
	ovnNodeSubnet, _ := ovn.ParseNodeHostSubnet(node)

	if !houtil.IsWindowsNode(node) {
		// Sync/remove subnet annotations for Linux nodes.
		// - if there is no OVN HostSubnet annotation, remove the HybridOverlayHostSubnet annotation
		// - if the OVN HostSubnet annotation is different than the HybridOverlayHostSubnet annotation,
		//   copy the OVN one to HybridOverlayHostSubnet
		if ovnNodeSubnet == nil {
			if extNodeSubnet != nil {
				// remove any HybridOverlayNodeSubnet
				logrus.Infof("Will remove node %s hybrid overlay NodeSubnet %s", node.Name, extNodeSubnet.String())
				annotator.Del(types.HybridOverlayNodeSubnet)
			}
		} else if !sameCIDR(ovnNodeSubnet, extNodeSubnet) {
			// sync the HybridOverlayNodeSubnet with the OVN-assigned one
			logrus.Infof("will sync node %s hybrid overlay NodeSubnet %s", node.Name, ovnNodeSubnet.String())
			annotator.Set(types.HybridOverlayNodeSubnet, ovnNodeSubnet.String())
		}
		return nil
	}

	// Do not allocate a subnet if the node already has one
	if extNodeSubnet != nil {
		return nil
	}

	// No subnet reserved; allocate a new one
	hostsubnetStr, err := m.allocator.AllocateNetwork()
	if err != nil {
		return fmt.Errorf("Error allocating hybrid overlay HostSubnet for node %s: %v", node.Name, err)
	}
	logrus.Infof("Allocated hybrid overlay HostSubnet %s for node %s", hostsubnetStr, node.Name)

	if _, _, err := net.ParseCIDR(hostsubnetStr); err != nil {
		return fmt.Errorf("Error parsing hostsubnet %s: %v", hostsubnetStr, err)
	}

	annotator.SetWithFailureHandler(types.HybridOverlayNodeSubnet, hostsubnetStr, func(node *kapi.Node, key string, val interface{}) {
		if _, cidr, _ := net.ParseCIDR(val.(string)); cidr != nil {
			_ = m.releaseNodeSubnet(node.Name, cidr)
		}
	})
	return nil
}

func (m *MasterController) releaseNodeSubnet(nodeName string, subnet *net.IPNet) error {
	err := m.allocator.ReleaseNetwork(subnet.String())
	if err != nil {
		return fmt.Errorf("Error deleting hybrid overlay HostSubnet %s for node %q: %s", subnet, nodeName, err)
	}
	logrus.Infof("Deleted hybrid overlay HostSubnet %s for node %s", subnet, nodeName)
	return nil
}

func (m *MasterController) handleOverlayPort(node *kapi.Node, annotator kube.Annotator) error {
	// Only applicable to Linux nodes
	if houtil.IsWindowsNode(node) {
		return nil
	}

	_, haveDRMACAnnotation := node.Annotations[types.HybridOverlayDrMac]

	subnet, err := ovn.ParseNodeHostSubnet(node)
	if subnet == nil || err != nil {
		// No subnet allocated yet; clean up
		if haveDRMACAnnotation {
			m.deleteOverlayPort(node)
			annotator.Del(types.HybridOverlayDrMac)
		}
		return nil
	}

	if haveDRMACAnnotation {
		// already set up; do nothing
		return nil
	}

	portName := houtil.GetHybridOverlayPortName(node.Name)
	portMAC, portIP, _ := util.GetPortAddresses(portName)
	if portMAC == nil || portIP == nil {
		if portMAC == nil {
			portMAC, _ = net.ParseMAC(util.GenerateMac())
		}
		if portIP == nil {
			// Get the 3rd address in the node's subnet; the first is taken
			// by the k8s-cluster-router port, the second by the management port
			first := util.NextIP(subnet.IP)
			second := util.NextIP(first)
			portIP = util.NextIP(second)
		}

		var stderr string
		_, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", node.Name, portName,
			"--", "lsp-set-addresses", portName, portMAC.String()+" "+portIP.String())
		if err != nil {
			return fmt.Errorf("failed to add hybrid overlay port for node %s"+
				", stderr:%s: %v", node.Name, stderr, err)
		}

		if err := util.UpdateNodeSwitchExcludeIPs(node.Name, subnet); err != nil {
			return err
		}
	}
	annotator.Set(types.HybridOverlayDrMac, portMAC.String())

	return nil
}

func (m *MasterController) deleteOverlayPort(node *kapi.Node) {
	portName := houtil.GetHybridOverlayPortName(node.Name)
	_, _, _ = util.RunOVNNbctl("--", "--if-exists", "lsp-del", portName)
}

// Add handles node additions
func (m *MasterController) Add(node *kapi.Node) {
	annotator := kube.NewNodeAnnotator(m.kube, node)

	if err := m.updateNodeAnnotation(node, annotator); err != nil {
		logrus.Errorf("failed to update node %q hybrid overlay subnet annotation: %v", node.Name, err)
	}

	if err := m.handleOverlayPort(node, annotator); err != nil {
		logrus.Errorf("failed to set up hybrid overlay logical switch port for %s: %v", node.Name, err)
	}

	annotator.Run()
}

// Update handles node updates
func (m *MasterController) Update(oldNode, newNode *kapi.Node) {
	m.Add(newNode)
}

// Delete handles node deletions
func (m *MasterController) Delete(node *kapi.Node) {
	// Run delete for all nodes in case the OS annotation was lost or changed

	if subnet, _ := parseHybridOverlayNodeHostSubnet(node); subnet != nil {
		if err := m.releaseNodeSubnet(node.Name, subnet); err != nil {
			logrus.Errorf(err.Error())
		}
	}

	if _, ok := node.Annotations[types.HybridOverlayDrMac]; ok {
		m.deleteOverlayPort(node)
	}
}

// Sync handles synchronizing the initial node list
func (m *MasterController) Sync(nodes []*kapi.Node) {
	// Unused because our initial node list sync needs to return
	// errors which this function cannot do
}
