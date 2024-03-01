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
// +kubebuilder:resource:path=qoses
// +kubebuilder::singular=qos
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// QoS is a CRD that allows the user to define a DSCP marking and metering
// for pods ingress/egress traffic on its namespace to specified CIDRs,
// protocol and port. Traffic belong these pods will be checked against
// each QoSRule in the namespace's QoS, and if there is a match the traffic
// is marked with relevant DSCP value and enforcing specified policing/shaping
// parameters.
type QoS struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Spec   `json:"spec,omitempty"`
	Status Status `json:"status,omitempty"`
}

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// Spec defines the desired state of QoS
type Spec struct {
	// a collection of Egress QoS rule objects
	Egress []Rule `json:"egress"`
}

type Rule struct {
	// MarkingDSCP marking value for matching pods' traffic.
	// +kubebuilder:validation:Maximum:=63
	// +kubebuilder:validation:Minimum:=0
	MarkingDSCP int `json:"markingDSCP"`

	// +optional
	Classifier Classifer `json:"classifier"`

	// +optional
	Policing Policing `json:"policing"`
}

// Classifer The classifier on which packets should match
// to apply the QoS Rule.
type Classifer struct {
	// CIDR specifies the destination's CIDR. Only traffic heading
	// to this CIDR will be marked with the DSCP value and policing/shaping
	// get applies when specified.
	// This field is optional, and in case it is not set the rule is applied
	// to all egress traffic regardless of the destination.
	// +optional
	// +kubebuilder:validation:Format="cidr"
	CIDR *string `json:"cidr,omitempty"`

	// protocol (tcp, udp, sctp) that the traffic must match.
	// +kubebuilder:validation:Pattern=^TCP|UDP|SCTP$
	// +optional
	Protocol string `json:"protocol"`

	// port that the traffic must match
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=65535
	// +optional
	Port int32 `json:"port"`
}

// Policing controls the maximum of rate traffic that can be sent
// or received on the matching packets.
type Policing struct {
	// Rate The value of rate limit in kbps. Traffic over the limit
	// will be dropped.
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=4294967295
	// +optional
	Rate uint32 `json:"rate"`

	// +optional
	Shaping Shaping `json:"shaping"`
}

// Shaping helps to control traffic flow by buffering packet bits when packet
// rate reaches the max limit and send it in next available slot.
type Shaping struct {
	// Burst The value of burst rate limit in kilobits.
	// This also needs rate to be specified.
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=4294967295
	// +optional
	Burst uint32 `json:"burst"`
}

// Status defines the observed state of QoS
type Status struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=qoses
// +kubebuilder::singular=qos
// QoSList contains a list of QoS
type QoSList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []QoS `json:"items"`
}
