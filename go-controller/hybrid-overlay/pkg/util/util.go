package util

import (
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// GetHybridOverlayPortName returns the name of the hybrid overlay switch port
// for a given node
func GetHybridOverlayPortName(nodeName string) string {
	return "int-" + nodeName
}

// IsWindowsNode returns true if the node has been labeled as a Windows node
func IsWindowsNode(node *kapi.Node) bool {
	osPlatform, ok := node.Labels[kapi.LabelOSStable]
	return ok && osPlatform == "windows"
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
			logrus.Debugf("node ADD event for %q", node.Name)
			h.Add(node)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode := oldObj.(*kapi.Node)
			newNode := newObj.(*kapi.Node)
			logrus.Debugf("node UPDATE event for %q", newNode.Name)
			h.Update(oldNode, newNode)
		},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("node DELETE event for %q", node.Name)
			h.Delete(node)
		},
	}, func(objs []interface{}) {
		logrus.Debugf("node SYNC event")
		nodeList := make([]*kapi.Node, 0, len(objs))
		for _, obj := range objs {
			node := obj.(*kapi.Node)
			nodeList = append(nodeList, node)
		}
		h.Sync(nodeList)
	})
	return err
}
