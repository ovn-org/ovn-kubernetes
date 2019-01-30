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
	return &nodeController{
		kube: &kube.Kube{KClient: clientset},
	}
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
