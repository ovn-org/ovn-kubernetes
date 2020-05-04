package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +resource:path=egressip
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// EgressIP is a CRD allowing the user to define a fixed
// source IP for all egress traffic originating from any pods which
// match the EgressIP resource according to its spec definition.
type EgressIP struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Specification of the desired behavior of EgressIP.
	Spec EgressIPSpec `json:"spec"`
	// Observed status of EgressIP
	// +optional
	Status EgressIPStatus `json:"status"`
}

// EgressIPStatus describes the lifecycle status of EgressIP.
type EgressIPStatus struct {
	EgressIPAssignments []EgressIPAssignment `json:"egressIPAssignments,omitempty"`
}

// EgressIPAssignment is the per node EgressIP status, for those who have been assigned one
type EgressIPAssignment struct {
	Node      string   `json:"node,omitempty"`
	EgressIPs []string `json:"egressIPs,omitempty"`
}

// EgressIPSpec is a desired state description of EgressIP.
type EgressIPSpec struct {
	// EgressCIDRs is the list of CIDR ranges available for automatically
	// assigning egress IPs to this node from. If this field is set then EgressIPs
	// should be treated as read-only.
	EgressCIDRs []string `json:"egressCIDRs,omitempty"`
	// EgressIPs is the list of automatic egress IP addresses currently
	// hosted by this node. If EgressCIDRs is empty, this can be set by hand;
	// if EgressCIDRs is set then the master will overwrite the value here with
	// its own allocation of egress IPs.
	EgressIPs []string `json:"egressIPs,omitempty"`
	// PodSelector applies the egress IP only to the pods whos label
	// matches this definition. This field is optional, and in case it's not set
	// results in the egress IP being applied to all pods in the namespace
	PodSelector PodSelector `json:"podSelector,omitempty"`
}

// matchLabels is a map of {key,value} pairs.
type PodSelector struct {
	// A single {key,value} in the
	// matchLabels map is matched against pod labels matching those.
	matchLabels map[string]string `json:"matchLabels"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +resource:path=egressip
// EgressIPList is the list of EgressIPList.
type EgressIPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	// List of EgressIP.
	Items []EgressIP `json:"items"`
}
