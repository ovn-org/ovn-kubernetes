package master

import (
	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/types"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	HybridClusterSubnet string
)

type MasterController struct {
	clientset *kubernetes.Clientset
}

func NewNodeHandler(clientset *kubernetes.Clientset) types.NodeHandler {
	return &MasterController{clientset: clientset}
}

func (m *MasterController) Add(node *kapi.Node) {
	return
}

func (m *MasterController) Update(oldNode, newNode *kapi.Node) {
	return
}

func (m *MasterController) Delete(node *kapi.Node) {
	return
}
