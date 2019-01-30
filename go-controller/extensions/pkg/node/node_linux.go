package node

import (
	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/types"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

type nodeController struct {
	kube *kube.Kube
}

func NewNodeHandler(clientset kubernetes.Interface) types.NodeHandler {
	n := &nodeController{
		kube: &kube.Kube{KClient: clientset},
	}
	// initialize self
	n.InitSelf()
	return n
}

// Add function learns about a new node being added to the cluster
// For a linux node, this means watching for other windows nodes
// and then programming the VxLAN gateway for the VTEP routes based on hostsubnet
// For a windows node, this means watching for all nodes and programming the routing
func (n *nodeController) Add(node *kapi.Node) {
	return
}

func (n *nodeController) Update(oldNode, newNode *kapi.Node) {
	return
}

func (n *nodeController) Delete(node *kapi.Node) {
	return
}

// InitSelf initializes the node it is currently running on.
// On Linux, this means:
//  1. Setting up a VxLAN gateway and hooking to the OVN gateway
//  2. Setting back annotations about its VTEP and gateway MAC address to its own object
func (n *nodeController) InitSelf() {
}
