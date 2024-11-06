package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ClusterUserDefinedNetwork describe network request for a shared network across namespaces.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=clusteruserdefinednetworks,scope=Cluster
// +kubebuilder:singular=clusteruserdefinednetwork
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type ClusterUserDefinedNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +required
	Spec ClusterUserDefinedNetworkSpec `json:"spec"`
	// +optional
	Status ClusterUserDefinedNetworkStatus `json:"status,omitempty"`
}

// ClusterUserDefinedNetworkSpec defines the desired state of ClusterUserDefinedNetwork.
type ClusterUserDefinedNetworkSpec struct {
	// NamespaceSelector Label selector for which namespace network should be available for.
	// +kubebuilder:validation:Required
	// +required
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// Network is the user-defined-network spec
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Network spec is immutable"
	// +required
	Network NetworkSpec `json:"network"`
}

// NetworkSpec defines the desired state of UserDefinedNetworkSpec.
// +union
type NetworkSpec struct {
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

// ClusterUserDefinedNetworkStatus contains the observed status of the ClusterUserDefinedNetwork.
type ClusterUserDefinedNetworkStatus struct {
	// Conditions slice of condition objects indicating details about ClusterUserDefineNetwork status.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterUserDefinedNetworkList contains a list of ClusterUserDefinedNetwork.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterUserDefinedNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterUserDefinedNetwork `json:"items"`
}
