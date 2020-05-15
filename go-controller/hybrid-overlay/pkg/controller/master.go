package controller

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/allocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog"
)

// MasterController is the master hybrid overlay controller
type MasterController struct {
	kube      kube.Interface
	allocator *allocator.SubnetAllocator
}

// NewMaster a new master controller that listens for node events
func NewMaster(kube kube.Interface) (*MasterController, error) {
	m := &MasterController{
		kube:      kube,
		allocator: allocator.NewSubnetAllocator(),
	}

	// Add our hybrid overlay CIDRs to the allocator
	for _, clusterEntry := range config.HybridOverlay.ClusterSubnets {
		err := m.allocator.AddNetworkRange(clusterEntry.CIDR, 32-clusterEntry.HostSubnetLength)
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
		hostsubnet, err := houtil.ParseHybridOverlayHostSubnet(&node)
		if err != nil {
			klog.Warningf(err.Error())
		} else if hostsubnet != nil {
			klog.V(5).Infof("marking existing node %s hybrid overlay NodeSubnet %s as allocated", node.Name, hostsubnet)
			if err := m.allocator.MarkAllocatedNetwork(hostsubnet); err != nil {
				utilruntime.HandleError(err)
			}
		}
	}

	return m, nil
}

// StartMaster creates and starts the hybrid overlay master controller
func StartMaster(kube kube.Interface, wf *factory.WatchFactory) error {
	klog.Infof("Starting hybrid overlay master...")
	master, err := NewMaster(kube)
	if err != nil {
		return err
	}
	return houtil.StartNodeWatch(master, wf)
}

// hybridOverlayNodeEnsureSubnet allocates a subnet and sets the
// hybrid overlay subnet annotation. It returns any newly allocated subnet
// or an error. If an error occurs, the newly allocated subnet will be released.
func (m *MasterController) hybridOverlayNodeEnsureSubnet(node *kapi.Node, annotator kube.Annotator) (*net.IPNet, error) {
	// Do not allocate a subnet if the node already has one
	if subnet, _ := houtil.ParseHybridOverlayHostSubnet(node); subnet != nil {
		return nil, nil
	}

	// Allocate a new host subnet for this node
	hostsubnets, err := m.allocator.AllocateNetworks()
	if err != nil {
		return nil, fmt.Errorf("Error allocating hybrid overlay HostSubnet for node %s: %v", node.Name, err)
	}

	if err := annotator.Set(types.HybridOverlayNodeSubnet, hostsubnets[0].String()); err != nil {
		_ = m.allocator.ReleaseNetwork(hostsubnets[0])
		return nil, err
	}

	klog.Infof("Allocated hybrid overlay HostSubnet %s for node %s", hostsubnets[0], node.Name)
	return hostsubnets[0], nil
}

func (m *MasterController) releaseNodeSubnet(nodeName string, subnet *net.IPNet) error {
	if err := m.allocator.ReleaseNetwork(subnet); err != nil {
		return fmt.Errorf("Error deleting hybrid overlay HostSubnet %s for node %q: %s", subnet, nodeName, err)
	}
	klog.Infof("Deleted hybrid overlay HostSubnet %s for node %s", subnet, nodeName)
	return nil
}

func (m *MasterController) handleOverlayPort(node *kapi.Node, annotator kube.Annotator) error {
	_, haveDRMACAnnotation := node.Annotations[types.HybridOverlayDRMAC]

	subnets, err := util.ParseNodeHostSubnetAnnotation(node)
	if subnets == nil || err != nil {
		// No subnet allocated yet; clean up
		if haveDRMACAnnotation {
			m.deleteOverlayPort(node)
			annotator.Delete(types.HybridOverlayDRMAC)
		}
		return nil
	}

	if haveDRMACAnnotation {
		// already set up; do nothing
		return nil
	}

	// FIXME DUAL-STACK
	subnet := subnets[0]

	portName := houtil.GetHybridOverlayPortName(node.Name)
	portMAC, portIP, _ := util.GetPortAddresses(portName)
	if portMAC == nil || portIP == nil {
		if portIP == nil {
			portIP = util.GetNodeHybridOverlayIfAddr(subnet).IP
		}
		if portMAC == nil {
			portMAC = util.IPAddrToHWAddr(portIP)
		}

		klog.Infof("creating node %s hybrid overlay port", node.Name)

		var stderr string
		_, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", node.Name, portName,
			"--", "lsp-set-addresses", portName, portMAC.String())
		if err != nil {
			return fmt.Errorf("failed to add hybrid overlay port for node %s"+
				", stderr:%s: %v", node.Name, stderr, err)
		}

		if err := util.UpdateNodeSwitchExcludeIPs(node.Name, subnet); err != nil {
			return err
		}
	}
	if err := annotator.Set(types.HybridOverlayDRMAC, portMAC.String()); err != nil {
		return fmt.Errorf("failed to set node %s hybrid overlay DRMAC annotation: %v", node.Name, err)
	}

	return nil
}

func (m *MasterController) deleteOverlayPort(node *kapi.Node) {
	klog.Infof("removing node %s hybrid overlay port", node.Name)
	portName := houtil.GetHybridOverlayPortName(node.Name)
	_, _, _ = util.RunOVNNbctl("--", "--if-exists", "lsp-del", portName)
}

// Add handles node additions
func (m *MasterController) Add(node *kapi.Node) {
	annotator := kube.NewNodeAnnotator(m.kube, node)

	var err error
	var allocatedSubnet *net.IPNet
	if houtil.IsHybridOverlayNode(node) {
		allocatedSubnet, err = m.hybridOverlayNodeEnsureSubnet(node, annotator)
		if err != nil {
			klog.Errorf("failed to update node %q hybrid overlay subnet annotation: %v", node.Name, err)
			return
		}
	} else {
		if err = m.handleOverlayPort(node, annotator); err != nil {
			klog.Errorf("failed to set up hybrid overlay logical switch port for %s: %v", node.Name, err)
			return
		}
	}

	if err = annotator.Run(); err != nil {
		// Release allocated subnet if any errors occurred
		if allocatedSubnet != nil {
			_ = m.releaseNodeSubnet(node.Name, allocatedSubnet)
		}
		klog.Errorf("failed to set hybrid overlay annotations for %s: %v", node.Name, err)
	}
}

// Update handles node updates
func (m *MasterController) Update(oldNode, newNode *kapi.Node) {
	m.Add(newNode)
}

// Delete handles node deletions
func (m *MasterController) Delete(node *kapi.Node) {
	if subnet, _ := houtil.ParseHybridOverlayHostSubnet(node); subnet != nil {
		if err := m.releaseNodeSubnet(node.Name, subnet); err != nil {
			klog.Errorf(err.Error())
		}
	}

	if _, ok := node.Annotations[types.HybridOverlayDRMAC]; ok && !houtil.IsHybridOverlayNode(node) {
		m.deleteOverlayPort(node)
	}
}

// Sync handles synchronizing the initial node list
func (m *MasterController) Sync(nodes []*kapi.Node) {
	// Unused because our initial node list sync needs to return
	// errors which this function cannot do
}
