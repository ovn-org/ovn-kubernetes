// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=observabilityconfigs,scope=Cluster,singular=observabilityconfig
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// ObservabilityConfig describes the OVN Observability API, which binds
// observed samples to a collector ID, for a given set of features and filters.
type ObservabilityConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +required
	Spec ObservabilitySpec `json:"spec"`
	// +optional
	Status ObservabilityStatus `json:"status,omitempty"`
}

// ObservabilitySpec defines the desired state of ObservabilityConfig.
// +kubebuilder:validation:XValidation:rule="!has(self.filter) || !has(self.filter.namespaces) || size(self.filter.namespaces) == 0 || self.features.all(f, f.feature == 'NetworkPolicy' || f.feature == 'EgressFirewall')",message="filter.namespaces can only be used with namespaced features (NetworkPolicy, EgressFirewall)"
type ObservabilitySpec struct {
	// CollectorID is the OVN Sample_Collector set_id used to bind samples to a collector on
	// the consumer side. It must be unique across the cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4294967295
	// +required
	CollectorID int64 `json:"collectorID"`
	// Features is a list of Observability features that can generate samples and their probabilities for a given collector.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=feature
	// +required
	Features []FeatureConfig `json:"features"`

	// Filter allows to apply ObservabilityConfig in a granular manner.
	// +optional
	Filter *Filter `json:"filter,omitempty"`
}

// FeatureConfig defines per-feature configuration, such as its sampling probability.
type FeatureConfig struct {
	// Probability is the probability of the feature being sampled in percent.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +required
	Probability int32 `json:"probability"`
	// Feature is the Observability feature that should be sampled.
	// +kubebuilder:validation:Required
	// +required
	Feature ObservabilityFeature `json:"feature"`
}

// Filter allows to apply ObservabilityConfig in a granular manner.
// Currently, it supports node and namespace based filtering.
// If both node and namespace filters are specified, they are logically ANDed.
// +kubebuilder:validation:MinProperties=1
type Filter struct {
	// nodeSelector applies ObservabilityConfig only to nodes that match the selector.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
	// namespaces is a list of namespaces to which the ObservabilityConfig should be applied.
	// It only applies to the namespaced features, currently that includes NetworkPolicy and EgressFirewall.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	// +listType=set
	Namespaces []string `json:"namespaces,omitempty"`
}

// ObservabilityStatus contains the observed status of the ObservabilityConfig.
type ObservabilityStatus struct {
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ObservabilityFeature is the type of OVN features that can be sampled for observability.
// +kubebuilder:validation:Enum=NetworkPolicy;AdminNetworkPolicy;EgressFirewall;UDNIsolation;MulticastIsolation
type ObservabilityFeature string

const (
	NetworkPolicy      ObservabilityFeature = "NetworkPolicy"
	AdminNetworkPolicy ObservabilityFeature = "AdminNetworkPolicy"
	EgressFirewall     ObservabilityFeature = "EgressFirewall"
	UDNIsolation       ObservabilityFeature = "UDNIsolation"
	MulticastIsolation ObservabilityFeature = "MulticastIsolation"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=observabilityconfigs,singular=observabilityconfig
// ObservabilityConfigList contains a list of ObservabilityConfigs.
type ObservabilityConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObservabilityConfig `json:"items"`
}
