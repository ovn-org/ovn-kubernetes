package util

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// ParseHybridOverlayHostSubnet returns the parsed hybrid overlay hostsubnet if
// the annotations included a valid one, or nil if they did not include one. If
// one was included, but it is invalid, an error is returned.
func ParseHybridOverlayHostSubnet(node *kapi.Node) (*net.IPNet, error) {
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

// GetHybridOverlayPortName returns the name of the hybrid overlay switch port
// for a given node
func GetHybridOverlayPortName(nodeName string) string {
	return "int-" + nodeName
}

// IsHybridOverlayNode returns true if the node has been labeled as a
// node which does not participate in the ovn-kubernetes overlay network
func IsHybridOverlayNode(node *kapi.Node) bool {
	if config.Kubernetes.NoHostSubnetNodes != nil {
		nodeSelector, _ := metav1.LabelSelectorAsSelector(config.Kubernetes.NoHostSubnetNodes)
		return nodeSelector.Matches(labels.Set(node.Labels))
	}
	return false
}

// SameIPNet returns true if both inputs are nil or if both inputs have the
// same value
func SameIPNet(a, b *net.IPNet) bool {
	if a == b {
		return true
	} else if a == nil || b == nil {
		return false
	}
	return a.String() == b.String()
}

// GetNodeInternalIP returns the first NodeInternalIP address of the node
func GetNodeInternalIP(node *kapi.Node) (string, error) {
	for _, addr := range node.Status.Addresses {
		if addr.Type == kapi.NodeInternalIP {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("failed to read node %q InternalIP", node.Name)
}

// StartNodeWatch starts a node event handler
func StartNodeWatch(h types.NodeHandler, wf *factory.WatchFactory) error {
	_, err := wf.AddNodeHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			h.Add(node)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode := oldObj.(*kapi.Node)
			newNode := newObj.(*kapi.Node)
			h.Update(oldNode, newNode)
		},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			h.Delete(node)
		},
	}, func(objs []interface{}) {
		nodeList := make([]*kapi.Node, 0, len(objs))
		for _, obj := range objs {
			node := obj.(*kapi.Node)
			nodeList = append(nodeList, node)
		}
		h.Sync(nodeList)
	})
	return err
}
