package util

import (
	"encoding/json"
	"fmt"
	"net"

	kapi "k8s.io/api/core/v1"
)

// This handles the annotations related to subnets assigned to a node. The annotations are
// created by the master, and then read by the node, and look like:
//
//   annotations:
//     k8s.ovn.org/node-subnets: |
//       {
//         "default": "10.130.0.0/23"
//       }
//     k8s.ovn.org/node-join-subnets: |
//       {
//         "default": "100.64.2.0/29"
//       }
//
// (This allows for specifying multiple network attachments, but currently only "default"
// is used.)

const (
	// OvnNodeSubnets is the constant string representing the node subnets annotation key
	OvnNodeSubnets = "k8s.ovn.org/node-subnets"
	// OvnNodeJoinSubnets is the constant string representing the node's join switch subnets annotation key
	OvnNodeJoinSubnets = "k8s.ovn.org/node-join-subnets"
)

// CreateNodeHostSubnetAnnotation creates a "k8s.ovn.org/node-subnets" annotation,
// with a single "default" network, suitable for passing to kube.SetAnnotationsOnNode
func CreateNodeHostSubnetAnnotation(defaultSubnet string) (map[string]interface{}, error) {
	bytes, err := json.Marshal(map[string]string{
		"default": defaultSubnet,
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		OvnNodeSubnets: string(bytes),
	}, nil
}

// ParseNodeHostSubnetAnnotation parses the "k8s.ovn.org/node-subnets" annotation
// on a node and returns the "default" host subnet.
func ParseNodeHostSubnetAnnotation(node *kapi.Node) (*net.IPNet, error) {
	sub, ok := node.Annotations[OvnNodeSubnets]
	if ok {
		nodeSubnets := make(map[string]string)
		if err := json.Unmarshal([]byte(sub), &nodeSubnets); err != nil {
			return nil, fmt.Errorf("error parsing node-subnets annotation: %v", err)
		}
		sub, ok = nodeSubnets["default"]
	}
	if !ok {
		return nil, fmt.Errorf("node %q has no host subnet annotation", node.Name)
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return nil, fmt.Errorf("error parsing host subnet: %v", err)
	}

	return subnet, nil
}

// CreateNodeJoinSubnetAnnotation creates a "k8s.ovn.org/node-join-subnets" annotation
// with a single "default" network, suitable for passing to kube.SetAnnotationsOnNode
func CreateNodeJoinSubnetAnnotation(defaultSubnet string) (map[string]interface{}, error) {
	bytes, err := json.Marshal(map[string]string{
		"default": defaultSubnet,
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		OvnNodeJoinSubnets: string(bytes),
	}, nil
}

// ParseNodeJoinSubnetAnnotation parses the "k8s.ovn.org/node-join-subnets" annotation on
// a node and returns the "default" join subnet.
func ParseNodeJoinSubnetAnnotation(node *kapi.Node) (*net.IPNet, error) {
	sub, ok := node.Annotations[OvnNodeJoinSubnets]
	if ok {
		nodeSubnets := make(map[string]string)
		if err := json.Unmarshal([]byte(sub), &nodeSubnets); err != nil {
			return nil, fmt.Errorf("error parsing node-join-subnets annotation: %v", err)
		}
		sub, ok = nodeSubnets["default"]
	}
	if !ok {
		return nil, fmt.Errorf("node %q has no join subnet annotation", node.Name)
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return nil, fmt.Errorf("error parsing join subnet: %v", err)
	}

	return subnet, nil
}
