package controller

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"

	"k8s.io/client-go/kubernetes"
)

// StartHybridOverlay starts one or both of the master and node controllers for
// hybrid overlay
func StartHybridOverlay(master bool, nodeName string, clientset kubernetes.Interface, wf *factory.WatchFactory) error {
	if master {
		masterController, err := NewMaster(clientset)
		if err != nil {
			return err
		}
		if err := masterController.Start(wf); err != nil {
			return err
		}
	}

	if nodeName != "" {
		nodeController, err := NewNode(clientset, nodeName)
		if err != nil {
			return err
		}
		if err := nodeController.Start(wf); err != nil {
			return err
		}
	}

	return nil
}
