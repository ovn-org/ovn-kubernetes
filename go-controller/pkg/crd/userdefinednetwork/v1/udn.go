package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// UserDefinedNetwork describe network request for a Namespace.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=userdefinednetworks,scope=Namespaced
// +kubebuilder:singular=userdefinednetwork
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type UserDefinedNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Spec is immutable"
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Layer3' ? has(self.layer3): !has(self.layer3)", message="spec.layer3 is required when topology is Layer3 and forbidden otherwise"
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Layer2' ? has(self.layer2): !has(self.layer2)", message="spec.layer2 is required when topology is Layer2 and forbidden otherwise"
	// +required
	Spec UserDefinedNetworkSpec `json:"spec"`
	// +optional
	Status UserDefinedNetworkStatus `json:"status,omitempty"`
}

// UserDefinedNetworkSpec defines the desired state of UserDefinedNetworkSpec.
// +union
type UserDefinedNetworkSpec struct {
	// Topology describes network configuration.
	//
	// Allowed values are "Layer3", "Layer2".
	// Layer3 topology creates a layer 2 segment per node, each with a different subnet. Layer 3 routing is used to interconnect node subnets.
	// Layer2 topology creates one logical switch shared by all nodes.
	//
	// +kubebuilder:validation:Required
	// +required
	// +unionDiscriminator
	Topology NetworkTopology `json:"topology"`

	// Layer3 is the Layer3 topology configuration.
	// +optional
	Layer3 *Layer3Config `json:"layer3,omitempty"`

	// Layer2 is the Layer2 topology configuration.
	// +optional
	Layer2 *Layer2Config `json:"layer2,omitempty"`
}

// UserDefinedNetworkStatus contains the observed status of the UserDefinedNetwork.
type UserDefinedNetworkStatus struct {
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// UserDefinedNetworkList contains a list of UserDefinedNetwork.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type UserDefinedNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UserDefinedNetwork `json:"items"`
}
