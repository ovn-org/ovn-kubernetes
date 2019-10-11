package controller

import (
	"bytes"
	"net"

	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"

	kapi "k8s.io/api/core/v1"
)

// nodeChanged returns true if any relevant node attributes changed
func nodeChanged(node1 *kapi.Node, node2 *kapi.Node) bool {
	cidr1, nodeIP1, drMAC1 := getNodeDetails(node1)
	cidr2, nodeIP2, drMAC2 := getNodeDetails(node2)

	if cidr1 != nil && cidr2 != nil && nodeIP1 != nil && nodeIP2 != nil && drMAC1 != nil && drMAC2 != nil {
		// The node was updated if either its subnet, its IP or its DRMAC has changed.
		return (!cidr1.IP.Equal(cidr2.IP) || !bytes.Equal(cidr1.Mask, cidr2.Mask) || !nodeIP1.Equal(nodeIP2) || !bytes.Equal(drMAC1, drMAC2))
	} else if cidr1 == nil && cidr2 == nil && nodeIP1 == nil && nodeIP2 == nil && drMAC1 == nil && drMAC2 == nil {
		return false
	}
	return true
}

// getNodeDetails reads and parses relevant node attributes and returns:
// 1) the node's hybrid overlay hostsubnet
// 2) the node's first InternalIP
// 3) the node's distributed router MAC (eg the MAC to which VXLAN packets should be sent)
func getNodeDetails(node *kapi.Node) (*net.IPNet, net.IP, net.HardwareAddr) {
	hostsubnet, ok := node.Annotations[types.HybridOverlayHostSubnet]
	if !ok {
		return nil, nil, nil
	}
	_, cidr, err := net.ParseCIDR(hostsubnet)
	if err != nil {
		logrus.Warningf("error parsing node %q subnet %q: %v", node.Name, hostsubnet, err)
		return nil, nil, nil
	}

	drMACString, ok := node.Annotations[types.HybridOverlayDrMac]
	if !ok {
		logrus.Warningf("missing node %q distributed router MAC annotation", node.Name)
		return nil, nil, nil
	}

	drMAC, err := net.ParseMAC(drMACString)
	if err != nil {
		logrus.Warningf("error parsing node %q distributed router MAC %q: %v", node.Name, drMACString, err)
		return nil, nil, nil
	}

	nodeIP, err := util.GetNodeInternalIP(node)
	if err != nil {
		logrus.Warningf("error getting node %q internal IP: %v", node.Name, err)
		return nil, nil, nil
	}

	return cidr, net.ParseIP(nodeIP), drMAC
}
