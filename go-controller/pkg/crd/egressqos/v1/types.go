/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=egressqoses
// +kubebuilder::singular=egressqos
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:subresource:status
// EgressQoS is a CRD that allows the user to define a DSCP value
// for pods egress traffic on its namespace to specified CIDRs.
// Traffic from these pods will be checked against each EgressQoSRule in
// the namespace's EgressQoS, and if there is a match the traffic is marked
// with the relevant DSCP value.
type EgressQoS struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EgressQoSSpec   `json:"spec,omitempty"`
	Status EgressQoSStatus `json:"status,omitempty"`
}

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// EgressQoSSpec defines the desired state of EgressQoS
type EgressQoSSpec struct {
	// a collection of Egress QoS rule objects
	Egress []EgressQoSRule `json:"egress"`
}

type EgressQoSRule struct {
	// DSCP marking value for matching pods' traffic.
	// +kubebuilder:validation:Maximum:=63
	// +kubebuilder:validation:Minimum:=0
	DSCP int `json:"dscp"`

	// DstCIDR specifies the destination's CIDR. Only traffic heading
	// to this CIDR will be marked with the DSCP value.
	// This field is optional, and in case it is not set the rule is applied
	// to all egress traffic regardless of the destination.
	// +optional
	// +kubebuilder:validation:Format="cidr"
	DstCIDR *string `json:"dstCIDR,omitempty"`

	// PodSelector applies the QoS rule only to the pods in the namespace whose label
	// matches this definition. This field is optional, and in case it is not set
	// results in the rule being applied to all pods in the namespace.
	// +optional
	PodSelector metav1.LabelSelector `json:"podSelector,omitempty"`
}

// EgressQoSStatus defines the observed state of EgressQoS
type EgressQoSStatus struct {
	// A concise indication of whether the EgressQoS resource is applied with success.
	// +optional
	Status string `json:"status,omitempty"`

	// An array of condition objects indicating details about status of EgressQoS object.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=egressqoses
// +kubebuilder::singular=egressqos
// EgressQoSList contains a list of EgressQoS
type EgressQoSList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressQoS `json:"items"`
}
