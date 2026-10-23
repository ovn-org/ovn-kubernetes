// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// HONodeController is the node hybrid overlay controller
// This controller will be running on hybrid overlay node. It is responsible for
//  1. Add Hybrid Overlay DRMAC annotation to its own node object,
//  2. Remove the ovnkube pod annotations from the pods running on this node.
type HONodeController struct {
	kube        kube.Interface
	nodeName    string
	localNodeIP net.IP
}

// newHONodeController returns a node handler that listens for node events
// so that Add/Update/Delete events are appropriately handled.
func newHONodeController(
	kube kube.Interface,
	nodeName string,
) (nodeController, error) {
	return &HONodeController{
		kube:     kube,
		nodeName: nodeName,
	}, nil
}

// AddNode set annotations about its VTEP and gateway MAC address to its own node object
func (n *HONodeController) AddNode(node *corev1.Node) error {
	if node.Name != n.nodeName {
		return nil
	}

	cidr, nodeIP := getNodeSubnetAndIP(node)
	if cidr == nil {
		return fmt.Errorf("failed to get hybrid overlay subnet the local node")
	}

	if nodeIP == nil {
		return fmt.Errorf("failed to get nodeIP of the local node")
	}

	if nodeIP.Equal(n.localNodeIP) {
		// Node IP doesn't changed. Skip updating hybrid overlay DRMAC annotation
		return nil
	}
	n.localNodeIP = nodeIP

	drMAC, err := getHostInterfaceMAC(n.localNodeIP)
	if err != nil {
		return err
	}

	if drMAC == "" {
		return fmt.Errorf("cannot to find the hybrid overlay distributed router gateway MAC address")
	}

	klog.Infof("Set hybrid overlay DRMAC annotation: %s", drMAC)
	if err := n.kube.SetAnnotationsOnNode(context.TODO(), node.Name, map[string]interface{}{
		hotypes.HybridOverlayDRMAC: drMAC,
	}); err != nil {
		return fmt.Errorf("failed to set DRMAC annotation on node: %v", err)
	}
	return nil
}

// Delete handles node deletions
func (n *HONodeController) DeleteNode(_ *corev1.Node) error {
	return nil
}

// AddPod remove the ovnkube annotation from the pods running on its own node
func (n *HONodeController) AddPod(pod *corev1.Pod) error {
	if pod.Spec.NodeName != n.nodeName {
		return nil
	}

	_, ok := pod.Annotations[types.OvnPodAnnotationName]
	if ok {
		klog.Infof("Remove the ovnkube pod annotation from pod %s", pod.Name)
		podToUpdate := pod.DeepCopy()
		delete(podToUpdate.Annotations, types.OvnPodAnnotationName)
		if err := n.kube.PatchPodStatusAnnotations(context.TODO(), pod, podToUpdate); err != nil {
			return fmt.Errorf("failed to remove ovnkube pod annotation from pod %s: %v", pod.Name, err)
		}
		return nil
	}
	return nil
}

func (n *HONodeController) DeletePod(_ *corev1.Pod) error {
	return nil
}

func (n *HONodeController) RunFlowSync(_ <-chan struct{}) {}

func (n *HONodeController) EnsureHybridOverlayBridge(_ *corev1.Node) error {
	return nil
}

func getHostInterfaceMAC(ip net.IP) (string, error) {
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return "", err
	}
	for _, link := range links {
		addrs, err := util.GetNetLinkOps().AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return "", fmt.Errorf("failed to get IP address for link %s: %v", link.Attrs().Name, err)
		}
		for _, add := range addrs {
			if add.IP.Equal(ip) {
				return link.Attrs().HardwareAddr.String(), nil
			}
		}
	}
	return "", fmt.Errorf("failed to get IP address for node IP: %s", ip)
}
