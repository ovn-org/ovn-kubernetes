package node

import (
	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/types"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

type NodeController struct {
	clientset *kubernetes.Clientset
}

func NewNodeHandler(clientset *kubernetes.Clientset) types.NodeHandler {
	return &NodeController{clientset: clientset}
}

func (n *NodeController) Add(node *kapi.Node) {
	return
}

func (n *NodeController) Update(oldNode, newNode *kapi.Node) {
	return
}

func (n *NodeController) Delete(node *kapi.Node) {
	return
}
