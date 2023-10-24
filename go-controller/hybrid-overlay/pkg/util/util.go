package util

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	kapi "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"
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
			return utilnet.ParseIPSloppy(addr.Address).String(), nil
		}
	}
	return "", fmt.Errorf("failed to read node %q InternalIP", node.Name)
}
