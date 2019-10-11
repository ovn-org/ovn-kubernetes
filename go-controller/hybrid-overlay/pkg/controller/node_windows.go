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

// Add is a stub that does nothing
func (n *NodeController) Add(node *kapi.Node) {
	return
}

// Update is a stub that does nothing
func (n *NodeController) Update(oldNode, newNode *kapi.Node) {
	return
}

// Delete is a stub that does nothing
func (n *NodeController) Delete(node *kapi.Node) {
	return
}

// Sync is a stub that does nothing
func (n *NodeController) Sync(nodes []*kapi.Node) {
}

// InitSelf is a stub that does nothing
func (n *NodeController) InitSelf() {
}
