// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	OVNNodeVTEPs = "k8s.ovn.org/vteps"
)

// VTEPNodeAnnotation holds the VTEP IPs discovered or allocated on a node.
// The annotation on the node is a JSON map keyed by VTEP name:
//
//	{"vtep-name1": {"ips": ["192.168.1.1", "fd00::1"]}, "vtep-name2": {"ips": ["10.0.0.1"]}}
type VTEPNodeAnnotation struct {
	IPs []string `json:"ips"`
}

// ParseNodeVTEPs parses the k8s.ovn.org/vteps annotation from a node and
// returns a map of VTEP name to its annotation data. Returns an
// AnnotationNotSetError if the annotation is not present.
func ParseNodeVTEPs(node *corev1.Node) (map[string]VTEPNodeAnnotation, error) {
	raw, ok := node.Annotations[OVNNodeVTEPs]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", OVNNodeVTEPs, node.Name)
	}
	var vteps map[string]VTEPNodeAnnotation
	if err := json.Unmarshal([]byte(raw), &vteps); err != nil {
		return nil, fmt.Errorf("failed to parse %s annotation on node %q: %w", OVNNodeVTEPs, node.Name, err)
	}
	return vteps, nil
}

// ValidateNodeVTEPs validates address syntax and family counts for selected VTEPs
// and returns canonical addresses. A nil selection validates all entries.
func ValidateNodeVTEPs(node *corev1.Node, selected sets.Set[string]) (map[string]VTEPNodeAnnotation, error) {
	raw, ok := node.Annotations[OVNNodeVTEPs]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", OVNNodeVTEPs, node.Name)
	}
	// Decode entries only after selection so unrelated malformed values cannot
	// prevent advertisements for the selected VTEPs.
	var entries map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("failed to parse %s annotation on node %q: %w", OVNNodeVTEPs, node.Name, err)
	}
	if selected != nil {
		maps.DeleteFunc(entries, func(name string, _ json.RawMessage) bool { return !selected.Has(name) })
	}
	vteps := make(map[string]VTEPNodeAnnotation, len(entries))
	// Return the same first validation error for unchanged annotations.
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		var entry VTEPNodeAnnotation
		if err := json.Unmarshal(entries[name], &entry); err != nil {
			return nil, fmt.Errorf("failed to parse %s annotation on node %q VTEP %q: %w", OVNNodeVTEPs, node.Name, name, err)
		}
		families := sets.New[bool]()
		for i, ip := range entry.IPs {
			addr, parseErr := netip.ParseAddr(ip)
			if parseErr != nil || addr.Zone() != "" || !addr.Unmap().IsGlobalUnicast() {
				return nil, fmt.Errorf("node %q VTEP %q has invalid IP %q", node.Name, name, ip)
			}
			addr = addr.Unmap()
			if families.Has(addr.Is6()) {
				return nil, fmt.Errorf("node %q VTEP %q has multiple IPs of the same family", node.Name, name)
			}
			families.Insert(addr.Is6())
			ip = addr.String()
			entry.IPs[i] = ip
		}
		vteps[name] = entry
	}
	return vteps, nil
}

// MarshalNodeVTEPs serializes the VTEPs annotation for use with SetAnnotationsOnNode.
// If the map is empty, the annotation value is set to nil so that a strategic
// merge patch removes the key from the node.
func MarshalNodeVTEPs(vteps map[string]VTEPNodeAnnotation) (map[string]interface{}, error) {
	if len(vteps) == 0 {
		return map[string]interface{}{OVNNodeVTEPs: nil}, nil
	}
	data, err := json.Marshal(vteps)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		OVNNodeVTEPs: string(data),
	}, nil
}

// NodeVTEPsAnnotationChanged returns true if the k8s.ovn.org/vteps annotation
// differs between the old and new node objects. Both oldNode and newNode must
// be non-nil;
func NodeVTEPsAnnotationChanged(oldNode, newNode *corev1.Node) bool {
	return oldNode.Annotations[OVNNodeVTEPs] != newNode.Annotations[OVNNodeVTEPs]
}
