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

// All fields in this package are required unless Explicitly marked optional
// +kubebuilder:validation:Required
package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AdminNetworkPolicySubject defines what resources the policy applies to.
// Exactly one field must be set.
// +kubebuilder:validation:MaxProperties=1
// +kubebuilder:validation:MinProperties=1
type AdminNetworkPolicySubject struct {
	// Namespaces is used to select pods via namespace selectors.
	// +optional
	Namespaces *metav1.LabelSelector `json:"namespaces,omitempty"`
	// Pods is used to select pods via namespace AND pod selectors.
	// +optional
	Pods *NamespacedPodSubject `json:"pods,omitempty"`
}

// NamespacedPodSubject allows the user to select a given set of pod(s) in
// selected namespace(s).
type NamespacedPodSubject struct {
	// NamespaceSelector follows standard label selector semantics; if empty,
	// it selects all Namespaces.
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// PodSelector is used to explicitly select pods within a namespace; if empty,
	// it selects all Pods.
	PodSelector metav1.LabelSelector `json:"podSelector"`
}

// AdminNetworkPolicyPort describes how to select network ports on pod(s).
// Exactly one field must be set.
// +kubebuilder:validation:MaxProperties=1
// +kubebuilder:validation:MinProperties=1
type AdminNetworkPolicyPort struct {
	// Port selects a port on a pod(s) based on number.
	// +optional
	PortNumber *Port `json:"portNumber,omitempty"`

	// NamedPort selects a port on a pod(s) based on name.
	// +optional
	NamedPort *string `json:"namedPort,omitempty"`

	// PortRange selects a port range on a pod(s) based on provided start and end
	// values.
	// +optional
	PortRange *PortRange `json:"portRange,omitempty"`
}

type Port struct {
	// Protocol is the network protocol (TCP, UDP, or SCTP) which traffic must
	// match. If not specified, this field defaults to TCP.
	Protocol v1.Protocol `json:"protocol"`

	// Number defines a network port value.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// PortRange defines an inclusive range of ports from the the assigned Start value
// to End value.
type PortRange struct {
	// Protocol is the network protocol (TCP, UDP, or SCTP) which traffic must
	// match. If not specified, this field defaults to TCP.
	Protocol v1.Protocol `json:"protocol,omitempty"`

	// Start defines a network port that is the start of a port range, the Start
	// value must be less than End.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Start int32 `json:"start"`

	// End defines a network port that is the end of a port range, the End value
	// must be greater than Start.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	End int32 `json:"end"`
}

// AdminNetworkPolicyPeer defines an in-cluster peer to allow traffic to/from.
// Exactly one of the selector pointers must be set for a given peer. If a
// consumer observes none of its fields are set, they must assume an unknown
// option has been specified and fail closed.
// +kubebuilder:validation:MaxProperties=1
// +kubebuilder:validation:MinProperties=1
type AdminNetworkPolicyPeer struct {
	// Namespaces defines a way to select a set of Namespaces.
	// +optional
	Namespaces *NamespacedPeer `json:"namespaces,omitempty"`
	// Pods defines a way to select a set of pods in
	// in a set of namespaces.
	// +optional
	Pods *NamespacedPodPeer `json:"pods,omitempty"`
}

type NamespaceRelation string

const (
	NamespaceSelf    NamespaceRelation = "Self"
	NamespaceNotSelf NamespaceRelation = "NotSelf"
)

// NamespacedPeer defines a flexible way to select Namespaces in a cluster.
// Exactly one of the selectors must be set.  If a consumer observes none of
// its fields are set, they must assume an unknown option has been specified
// and fail closed.
// +kubebuilder:validation:MaxProperties=1
// +kubebuilder:validation:MinProperties=1
type NamespacedPeer struct {
	// Related provides a mechanism for selecting namespaces relative to the
	// subject pod. A value of "Self" matches the subject pod's namespace,
	// while a value of "NotSelf" matches namespaces other than the subject
	// pod's namespace.
	// +optional
	Related *NamespaceRelation `json:"related,omitempty"`

	// NamespaceSelector is a labelSelector used to select Namespaces, This field
	// follows standard label selector semantics; if present but empty, it selects
	// all Namespaces.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// SameLabels is used to select a set of Namespaces that share the same values
	// for a set of labels.
	// To be selected a Namespace must have all of the labels defined in SameLabels,
	// and they must all have the same value as the subject of this policy.
	// If Samelabels is Empty then nothing is selected.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	SameLabels []string `json:"sameLabels,omitempty"`

	// NotSameLabels is used to select a set of Namespaces that do not have a set
	// of label(s). To be selected a Namespace must have none of the labels defined
	// in NotSameLabels. If NotSameLabels is empty then nothing is selected.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	NotSameLabels []string `json:"notSameLabels,omitempty"`
}

// NamespacedPodPeer defines a flexible way to select Namespaces and pods in a
// cluster. The `Namespaces` and `PodSelector` fields are required.
type NamespacedPodPeer struct {
	// Namespaces is used to select a set of Namespaces.
	Namespaces NamespacedPeer `json:"namespaces"`

	// PodSelector is a labelSelector used to select Pods, This field is NOT optional,
	// follows standard label selector semantics and if present but empty, it selects
	// all Pods.
	PodSelector metav1.LabelSelector `json:"podSelector"`
}
