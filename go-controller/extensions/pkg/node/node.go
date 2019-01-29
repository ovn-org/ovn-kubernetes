package node

import (
	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/types"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

type nodeController struct {
	clientset *kubernetes.Clientset
}

func NewNodeHandler(clientset *kubernetes.Clientset) types.NodeHandler {
	return &nodeController{clientset: clientset}
}

func (n *nodeController) Add(node *kapi.Node) {
	return
}

func (n *nodeController) Update(oldNode, newNode *kapi.Node) {
	return
}

func (n *nodeController) Delete(node *kapi.Node) {
	return
}
