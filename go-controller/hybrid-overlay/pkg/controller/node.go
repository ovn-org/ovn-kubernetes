package controller

import (
	"fmt"
	"net"
	"reflect"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
)

// nodeChanged returns true if any relevant node attributes changed
func nodeChanged(node1 *kapi.Node, node2 *kapi.Node) bool {
	cidr1, nodeIP1, drMAC1, _ := getNodeDetails(node1)
	cidr2, nodeIP2, drMAC2, _ := getNodeDetails(node2)
	return !reflect.DeepEqual(cidr1, cidr2) || !reflect.DeepEqual(nodeIP1, nodeIP2) || !reflect.DeepEqual(drMAC1, drMAC2)
}

// getNodeSubnetAndIP returns the node's hybrid overlay subnet and the node's
// first InternalIP, or nil if the subnet or node IP is invalid
func getNodeSubnetAndIP(node *kapi.Node) (*net.IPNet, net.IP) {
	// Parse Linux node OVN hostsubnet annotation first
	cidr, _ := util.ParseNodeHostSubnetAnnotation(node)
	if cidr == nil {
		// Otherwise parse the hybrid overlay node subnet annotation
		subnet, ok := node.Annotations[types.HybridOverlayNodeSubnet]
		if !ok {

			klog.V(5).Infof("missing node %q node subnet annotation", node.Name)
			return nil, nil
		}
		var err error
		_, cidr, err = net.ParseCIDR(subnet)
		if err != nil {
			klog.Errorf("error parsing node %q subnet %q: %v", node.Name, subnet, err)
			return nil, nil
		}
	}

	nodeIP, err := houtil.GetNodeInternalIP(node)
	if err != nil {
		klog.Errorf("error getting node %q internal IP: %v", node.Name, err)
		return nil, nil
	}

	return cidr, net.ParseIP(nodeIP)
}

// getNodeDetails returns the node's hybrid overlay subnet, first InternalIP,
// and the distributed router MAC (DRMAC), or nil if any of the addresses are
// missing or invalid.
func getNodeDetails(node *kapi.Node) (*net.IPNet, net.IP, net.HardwareAddr, error) {
	cidr, ip := getNodeSubnetAndIP(node)
	if cidr == nil || ip == nil {
		return nil, nil, nil, fmt.Errorf("missing node subnet and/or node IP")
	}

	drMACString, ok := node.Annotations[types.HybridOverlayDRMAC]
	if !ok {
		return nil, nil, nil, fmt.Errorf("missing distributed router MAC annotation")
	}
	drMAC, err := net.ParseMAC(drMACString)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid distributed router MAC %q: %v", drMACString, err)
	}

	return cidr, ip, drMAC, nil
}
