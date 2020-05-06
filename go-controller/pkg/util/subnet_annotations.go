package util

import (
	"encoding/json"
	"fmt"
	"net"

	kapi "k8s.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
)

// This handles the annotations related to subnets assigned to a node. The annotations are
// created by the master, and then read by the node. In a single-stack cluster, they look
// like:
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
//
// In a dual-stack cluster, the values are lists:
//
//   annotations:
//     k8s.ovn.org/node-subnets: |
//       {
//         "default": ["10.130.0.0/23", "fd01:0:0:2::/64"]
//       }
//     k8s.ovn.org/node-join-subnets: |
//       {
//         "default": ["100.64.2.0/29", "fd99::10/125"]
//       }

const (
	// ovnNodeSubnets is the constant string representing the node subnets annotation key
	ovnNodeSubnets = "k8s.ovn.org/node-subnets"
	// ovnNodeJoinSubnets is the constant string representing the node's join switch subnets annotation key
	ovnNodeJoinSubnets = "k8s.ovn.org/node-join-subnets"
)

func createSubnetAnnotation(annotationName string, defaultSubnets []*net.IPNet) (map[string]interface{}, error) {
	var bytes []byte
	var err error

	if len(defaultSubnets) == 1 {
		bytes, err = json.Marshal(map[string]string{
			"default": defaultSubnets[0].String(),
		})
	} else {
		defaultSubnetStrs := make([]string, len(defaultSubnets))
		for i := range defaultSubnets {
			defaultSubnetStrs[i] = defaultSubnets[i].String()
		}
		bytes, err = json.Marshal(map[string][]string{
			"default": defaultSubnetStrs,
		})
	}
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		annotationName: string(bytes),
	}, nil
}

func setSubnetAnnotation(nodeAnnotator kube.Annotator, annotationName string, defaultSubnets []*net.IPNet) error {
	annotation, err := createSubnetAnnotation(annotationName, defaultSubnets)
	if err != nil {
		return err
	}
	return nodeAnnotator.Set(annotationName, annotation[annotationName])
}

func parseSubnetAnnotation(node *kapi.Node, annotationName string) ([]*net.IPNet, error) {
	annotation, ok := node.Annotations[annotationName]
	if !ok {
		return nil, fmt.Errorf("node %q has no %q annotation", node.Name, annotationName)
	}

	var subnets []string
	subnetsDual := make(map[string][]string)
	if err := json.Unmarshal([]byte(annotation), &subnetsDual); err == nil {
		subnets, ok = subnetsDual["default"]
	} else {
		subnetsSingle := make(map[string]string)
		if err := json.Unmarshal([]byte(annotation), &subnetsSingle); err != nil {
			return nil, fmt.Errorf("could not parse %q annotation %q as either single-stack or dual-stack",
				annotationName, annotation)
		}
		subnets = make([]string, 1)
		subnets[0], ok = subnetsSingle["default"]
	}
	if !ok {
		return nil, fmt.Errorf("%q annotation %q has no default network", annotationName, annotation)
	}

	var ipnets []*net.IPNet
	for _, subnet := range subnets {
		_, ipnet, err := net.ParseCIDR(subnet)
		if err != nil {
			return nil, fmt.Errorf("error parsing %q value: %v", annotationName, err)
		}
		ipnets = append(ipnets, ipnet)
	}

	return ipnets, nil
}

// CreateNodeHostSubnetAnnotation creates a "k8s.ovn.org/node-subnets" annotation,
// with a single "default" network, suitable for passing to kube.SetAnnotationsOnNode
func CreateNodeHostSubnetAnnotation(defaultSubnets []*net.IPNet) (map[string]interface{}, error) {
	return createSubnetAnnotation(ovnNodeSubnets, defaultSubnets)
}

// SetNodeHostSubnetAnnotation sets a "k8s.ovn.org/node-subnets" annotation
// using a kube.Annotator
func SetNodeHostSubnetAnnotation(nodeAnnotator kube.Annotator, defaultSubnets []*net.IPNet) error {
	return setSubnetAnnotation(nodeAnnotator, ovnNodeSubnets, defaultSubnets)
}

// DeleteNodeHostSubnetAnnotation removes a "k8s.ovn.org/node-subnets" annotation
// using a kube.Annotator
func DeleteNodeHostSubnetAnnotation(nodeAnnotator kube.Annotator) {
	nodeAnnotator.Delete(ovnNodeSubnets)
}

// ParseNodeHostSubnetAnnotation parses the "k8s.ovn.org/node-subnets" annotation
// on a node and returns the "default" host subnet.
func ParseNodeHostSubnetAnnotation(node *kapi.Node) ([]*net.IPNet, error) {
	return parseSubnetAnnotation(node, ovnNodeSubnets)
}

// CreateNodeJoinSubnetAnnotation creates a "k8s.ovn.org/node-join-subnets" annotation,
// with a single "default" network, suitable for passing to kube.SetAnnotationsOnNode
func CreateNodeJoinSubnetAnnotation(defaultSubnets []*net.IPNet) (map[string]interface{}, error) {
	return createSubnetAnnotation(ovnNodeJoinSubnets, defaultSubnets)
}

// SetNodeJoinSubnetAnnotation sets a "k8s.ovn.org/node-join-subnets" annotation
// using a kube.Annotator
func SetNodeJoinSubnetAnnotation(nodeAnnotator kube.Annotator, defaultSubnets []*net.IPNet) error {
	return setSubnetAnnotation(nodeAnnotator, ovnNodeJoinSubnets, defaultSubnets)
}

// ParseNodeJoinSubnetAnnotation parses the "k8s.ovn.org/node-join-subnets" annotation on
// a node and returns the "default" join subnet.
func ParseNodeJoinSubnetAnnotation(node *kapi.Node) ([]*net.IPNet, error) {
	return parseSubnetAnnotation(node, ovnNodeJoinSubnets)
}
