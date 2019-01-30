package master

import (
	"net"

	"github.com/sirupsen/logrus"

	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/types"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"

	"github.com/openshift/origin/pkg/util/netutils"
	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	ExtensionHostSubnet = "hybrid-sdn.extensions.ovn.kubernets.io/hostsubnet"
	KubeOSKey           = "beta.kubernetes.io/os"
)

var (
	HybridClusterSubnet string
)

type masterController struct {
	kube                  *kube.Kube
	masterSubnetAllocator *netutils.SubnetAllocator
}

func NewNodeHandler(clientset kubernetes.Interface) types.NodeHandler {
	m := &masterController{
		kube: &kube.Kube{KClient: clientset},
	}
	// TODO: init subrange with all existing windows node
	// for the case when this software is restarted
	subrange := make([]string, 0)
	subnetAllocator, err := netutils.NewSubnetAllocator(HybridClusterSubnet, 8, subrange)
	if err != nil {
		logrus.Errorf("Error initializing allocator, Is subnet %s good enough? (%v)", HybridClusterSubnet, err)
		return nil
	}
	m.masterSubnetAllocator = subnetAllocator
	return m
}

func (m *masterController) Add(node *kapi.Node) {
	// check if this is a node that we care about
	// for the hybrid-sdn case, we care about it only if its a windows node
	osPlatform, ok := node.Annotations[KubeOSKey]
	if !ok || osPlatform != "Windows" {
		return
	}

	// Do not create a subnet if the node already has a subnet
	hostsubnet, ok := node.Annotations[ExtensionHostSubnet]
	if ok {
		// double check if the hostsubnet looks valid
		_, _, err := net.ParseCIDR(hostsubnet)
		if err == nil {
			return
		}
	}

	// Create new subnet
	sn, err := m.masterSubnetAllocator.GetNetwork()
	if err != nil {
		logrus.Errorf("Error allocating network for node %s: %v", node.Name, err)
		return
	} else {
		err = m.kube.SetAnnotationOnNode(node, ExtensionHostSubnet, sn.String())
		if err != nil {
			_ = m.masterSubnetAllocator.ReleaseNetwork(sn)
			logrus.Errorf("Error creating subnet %s for node %s: %v", sn.String(), node.Name, err)
			return
		}
		logrus.Infof("Created HostSubnet %s", sn.String())
		return
	}
}

func (m *masterController) Update(oldNode, newNode *kapi.Node) {
	return
}

func (m *masterController) Delete(node *kapi.Node) {
	sub, ok := node.Annotations[ExtensionHostSubnet]
	if !ok {
		logrus.Errorf("Error in obtaining host subnet for node %q for deletion", node.Name)
		return
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		logrus.Errorf("Error in parsing hostsubnet - %v", err)
		return
	}
	err = m.masterSubnetAllocator.ReleaseNetwork(subnet)
	if err == nil {
		logrus.Infof("Deleted HostSubnet %s for node %s", sub, node.Name)
		return
	}
	logrus.Errorf("Error deleting subnet %v for node %q: subnet not found in any CIDR range or already available", sub, node.Name)

	return
}
