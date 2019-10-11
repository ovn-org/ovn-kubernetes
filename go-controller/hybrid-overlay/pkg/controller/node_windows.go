package controller

import (
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// NodeController is the node hybrid overlay controller
type NodeController struct {
	kube *kube.Kube
}

// NewNode returns a node handler that listens for node events
// so that Add/Update/Delete events are appropriately handled.
func NewNode(clientset kubernetes.Interface, nodeName string) (*NodeController, error) {
	n := &NodeController{
		kube: &kube.Kube{KClient: clientset},
	}
	// initialize self
	n.InitSelf()
	return n, nil
}

// Start is the top level function to run hybrid-sdn in node mode
func (n *NodeController) Start(wf *factory.WatchFactory) error {
	return houtil.StartNodeWatch(n, wf)
}

// Add function learns about a new node being added to the cluster
// For a windows node, this means watching for all nodes and programming the routing
func (n *NodeController) Add(node *kapi.Node) {
	return
}

// Update handles node updates
func (n *NodeController) Update(oldNode, newNode *kapi.Node) {
	return
}

// Delete handles node deletions
func (n *NodeController) Delete(node *kapi.Node) {
	return
}

// Sync handles synchronizing the initial node list
func (n *NodeController) Sync(nodes []*kapi.Node) {
}

// InitSelf initializes the node it is currently running on.
// On Windows, this means:
//  1. Setting up this node and its VXLAN extension for talking to other nodes
//  2. Setting back annotations about its VTEP and gateway MAC address to its own node object
func (n *NodeController) InitSelf() {
}
