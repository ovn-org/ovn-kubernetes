package util

import (
	"net"
	"reflect"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// NoHostSubnet compares the no-hostsubnet-nodes flag with node labels to see if the node is managing its
// own network.
func NoHostSubnet(node *v1.Node) bool {
	if config.Kubernetes.NoHostSubnetNodes == nil {
		return false
	}

	nodeSelector, _ := metav1.LabelSelectorAsSelector(config.Kubernetes.NoHostSubnetNodes)
	return nodeSelector.Matches(labels.Set(node.Labels))
}

func NodeSubnetChanged(oldNode, node *v1.Node) bool {
	oldSubnets, _ := util.ParseNodeHostSubnetAnnotation(oldNode, types.DefaultNetworkName)
	newSubnets, _ := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	return !reflect.DeepEqual(oldSubnets, newSubnets)
}

func NodeChassisChanged(oldNode, node *v1.Node) bool {
	oldChassis, _ := util.ParseNodeChassisIDAnnotation(oldNode)
	newChassis, _ := util.ParseNodeChassisIDAnnotation(node)
	return oldChassis != newChassis
}

func CreateIPAddressSlice(ips []*net.IPNet) []net.IP {
	ipAddrs := make([]net.IP, 0)
	for _, ip := range ips {
		ipAddrs = append(ipAddrs, ip.IP)
	}
	return ipAddrs
}
