package util

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"

	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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

const (
	// ovnNodeSubnets is the constant string representing the node subnets annotation key
	ovnNodeSubnets = "k8s.ovn.org/node-subnets"
)

// updateSubnetAnnotation add the hostSubnets of the given network to the input node annotations;
// input annotations is not nil
// if hostSubnets is empty, deletes the existing subnet annotation for given network from the input node annotations.
func updateSubnetAnnotation(annotations map[string]string, annotationName, netName string, hostSubnets []*net.IPNet) error {
	var bytes []byte

	// First get the all host subnets for all existing networks
	subnetsMap, err := parseSubnetAnnotation(annotations, annotationName)
	if err != nil {
		if !IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to parse node subnet annotation %q: %v",
				annotations, err)
		}
		// in the case that the annotation does not exist
		subnetsMap = map[string][]*net.IPNet{}
	}

	// add or delete host subnet of the specified network
	if len(hostSubnets) != 0 {
		subnetsMap[netName] = hostSubnets
	} else {
		delete(subnetsMap, netName)
	}

	// if no host subnet left, just delete the host subnet annotation from node annotations.
	if len(subnetsMap) == 0 {
		delete(annotations, annotationName)
		return nil
	}

	// Marshal all host subnets of all networks back to annotations.
	subnetsStrMap := make(map[string][]string)
	for n, subnets := range subnetsMap {
		subnetsStr := make([]string, len(subnets))
		for i, subnet := range subnets {
			subnetsStr[i] = subnet.String()
		}
		subnetsStrMap[n] = subnetsStr
	}
	bytes, err = json.Marshal(subnetsStrMap)
	if err != nil {
		return err
	}
	annotations[annotationName] = string(bytes)
	return nil
}

func setSubnetAnnotation(nodeAnnotator kube.Annotator, annotationName string, defaultSubnets []*net.IPNet) error {
	annotation := map[string]string{}
	err := updateSubnetAnnotation(annotation, annotationName, types.DefaultNetworkName, defaultSubnets)
	if err != nil {
		return err
	}
	return nodeAnnotator.Set(annotationName, annotation[annotationName])
}

func parseSubnetAnnotation(nodeAnnotations map[string]string, annotationName string) (map[string][]*net.IPNet, error) {
	annotation, ok := nodeAnnotations[annotationName]
	if !ok {
		return nil, newAnnotationNotSetError("could not find %q annotation", annotationName)
	}
	subnetsStrMap := map[string][]string{}
	subnetsDual := make(map[string][]string)
	if err := json.Unmarshal([]byte(annotation), &subnetsDual); err == nil {
		subnetsStrMap = subnetsDual
	} else {
		subnetsSingle := make(map[string]string)
		if err := json.Unmarshal([]byte(annotation), &subnetsSingle); err != nil {
			return nil, fmt.Errorf("could not parse %q annotation %q as either single-stack or dual-stack: %v",
				annotationName, annotation, err)
		}
		for netName, v := range subnetsSingle {
			subnetsStrMap[netName] = make([]string, 1)
			subnetsStrMap[netName][0] = v
		}
	}

	if len(subnetsStrMap) == 0 {
		return nil, fmt.Errorf("unexpected empty %s annotation", annotationName)
	}

	subnetMap := make(map[string][]*net.IPNet)
	for netName, subnetsStr := range subnetsStrMap {
		var ipnets []*net.IPNet
		for _, subnet := range subnetsStr {
			_, ipnet, err := net.ParseCIDR(subnet)
			if err != nil {
				return nil, fmt.Errorf("error parsing %q value: %v", annotationName, err)
			}
			ipnets = append(ipnets, ipnet)
		}
		subnetMap[netName] = ipnets
	}

	return subnetMap, nil
}

func NodeSubnetAnnotationChanged(oldNode, newNode *v1.Node) bool {
	return oldNode.Annotations[ovnNodeSubnets] != newNode.Annotations[ovnNodeSubnets]
}

// ExtractJSONStringValue tries to extract a JSON string value for the provided key.
// It is faster than unmarshalling but should not be used unless strictly necessary.
// The function only looks for quoted values.
// TODO: this function assumes valid JSON, it doesn't work for arrays yet
func ExtractJSONStringValue(rawJSON, key string) (string, error) {
	re := regexp.MustCompile(`"` + key + `"\s*:\s*"(.*?)"`)
	matches := re.FindStringSubmatch(rawJSON)
	if len(matches) != 2 {
		return "", fmt.Errorf("failed to extract JSON string value %v from %q", matches, key)
	}
	return matches[1], nil
}

func ExtractJSONArrayValue(rawJSON, key string) (string, error) {
	re := regexp.MustCompile(`"` + key + `"\s*:\s*\[(.*?)\]`)
	matches := re.FindStringSubmatch(rawJSON)
	if len(matches) != 2 {
		return "", fmt.Errorf("failed to extract JSON string value %v from %q", matches, key)
	}
	return matches[1], nil
}

// oldSubnet, err := ExtractJSONStringValue(oldNode.Annotations[ovnNodeSubnets], netName)
//
//	if err != nil {
//		klog.Errorf("%s", err)
//		return true
//	}
//
// newSubnet, err := ExtractJSONStringValue(newNode.Annotations[ovnNodeSubnets], netName)
//
//	if err != nil {
//		klog.Errorf("%s", err)
//		return true
//	}
//
// return oldSubnet != newSubnet
func NodeSubnetAnnotationChangedForNetwork(oldNode, newNode *v1.Node, netName string) bool {
	oldSubnet, _ := ExtractJSONArrayValue(oldNode.Annotations[ovnNodeSubnets], netName)
	newSubnet, _ := ExtractJSONArrayValue(newNode.Annotations[ovnNodeSubnets], netName)
	return oldSubnet != newSubnet
}

// UpdateNodeHostSubnetAnnotation updates a "k8s.ovn.org/node-subnets" annotation for network "netName",
// with the specified network, suitable for passing to kube.SetAnnotationsOnNode. If hostSubnets is empty,
// it deleted the "k8s.ovn.org/node-subnets" annotation for network "netName"
func UpdateNodeHostSubnetAnnotation(annotations map[string]string, hostSubnets []*net.IPNet, netName string) (map[string]string, error) {
	if annotations == nil {
		annotations = map[string]string{}
	}
	err := updateSubnetAnnotation(annotations, ovnNodeSubnets, netName, hostSubnets)
	if err != nil {
		return nil, err
	}
	return annotations, nil
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
// on a node and returns the host subnet for the given network.
func ParseNodeHostSubnetAnnotation(node *kapi.Node, netName string) ([]*net.IPNet, error) {
	var nodeSubnetMap map[string]json.RawMessage
	var ret []*net.IPNet
	annotation, ok := node.Annotations[ovnNodeSubnets]
	if !ok {
		return nil, newAnnotationNotSetError("could not find %q annotation", ovnNodeSubnets)
	}
	if err := json.Unmarshal([]byte(annotation), &nodeSubnetMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %q annotation on node %s: %v", ovnNodeSubnets, node.Name, err)
	}
	val, ok := nodeSubnetMap[netName]
	if !ok {
		return nil, newAnnotationNotSetError("node %q has no %q annotation for network %s", node.Name, ovnNodeSubnets, netName)
	}

	var subnets, subnetsDual []string
	// TODO: Is this really what we need to do?
	if err := json.Unmarshal(val, &subnetsDual); err == nil {
		subnets = subnetsDual
	} else {
		subnetsSingle := ""
		if err := json.Unmarshal(val, &subnetsSingle); err != nil {
			return nil, fmt.Errorf("could not parse %q annotation %q as either single-stack or dual-stack: %v",
				ovnNodeSubnets, val, err)
		}
		subnets = append(subnets, subnetsSingle)
	}

	if len(subnets) == 0 {
		return nil, fmt.Errorf("unexpected empty %s annotation for %s network", ovnNodeSubnets, netName)
	}

	for _, subnet := range subnets {
		_, ipnet, err := net.ParseCIDR(subnet)
		if err != nil {
			return nil, fmt.Errorf("error parsing %q value: %v", subnet, err)
		}
		ret = append(ret, ipnet)
	}

	return ret, nil
}

// GetNodeSubnetAnnotationNetworkNames parses the "k8s.ovn.org/node-subnets" annotation
// on a node and returns the list of network names set.
func GetNodeSubnetAnnotationNetworkNames(node *kapi.Node) ([]string, error) {
	nodeNetworks := []string{}
	subnetsMap, err := parseSubnetAnnotation(node.Annotations, ovnNodeSubnets)
	if err != nil {
		return nodeNetworks, err
	}

	for network := range subnetsMap {
		nodeNetworks = append(nodeNetworks, network)
	}

	return nodeNetworks, nil
}

// ParseNodesHostSubnetAnnotation parses parses the "k8s.ovn.org/node-subnets" annotation
// for all the provided nodes
func ParseNodesHostSubnetAnnotation(nodes []*kapi.Node, netName string) ([]*net.IPNet, error) {
	allSubnets := []*net.IPNet{}
	for _, node := range nodes {
		subnets, err := ParseNodeHostSubnetAnnotation(node, netName)
		if err != nil {
			return nil, err
		}
		allSubnets = append(allSubnets, subnets...)
	}
	return allSubnets, nil
}
