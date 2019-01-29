package master

import (
	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/types"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	HybridClusterSubnet string
)

type masterController struct {
	clientset *kubernetes.Clientset
}

func NewNodeHandler(clientset *kubernetes.Clientset) types.NodeHandler {
	return &masterController{clientset: clientset}
}

func (m *masterController) Add(node *kapi.Node) {
	return
}

func (m *masterController) Update(oldNode, newNode *kapi.Node) {
	return
}

func (m *masterController) Delete(node *kapi.Node) {
	return
}
