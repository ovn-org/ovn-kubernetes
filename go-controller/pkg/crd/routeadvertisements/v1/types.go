package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:openapi-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=routeadvertisements,scope=Cluster,shortName=ra,singular=routeadvertisements
// +kubebuilder::singular=routeadvertisements
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// RouteAdvertisements is the Schema for the routeadvertisements API
type RouteAdvertisements struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RouteAdvertisementsSpec   `json:"spec,omitempty"`
	Status RouteAdvertisementsStatus `json:"status,omitempty"`
}

// RouteAdvertisementsSpec defines the desired state of RouteAdvertisements
type RouteAdvertisementsSpec struct {
	// TargetVRF determines which VRF the routes should be advertised in.
	// +kubebuilder:validation:Optional
	TargetVRF string `json:"targetVRF,omitempty"`

	// NetworkSelector determines which network routes should be advertised.
	// When omitted, the default cluster network routes are advertised.
	// +kubebuilder:validation:Optional
	NetworkSelector *metav1.LabelSelector `json:"networkSelector,omitempty"`

	// NodeSelector limits the advertisements to selected nodes.
	// +kubebuilder:validation:Required
	NodeSelector metav1.LabelSelector `json:"nodeSelector,omitempty"`

	// FrrConfigurationSelector determines which FRRConfiguration will the
	// OVN-Kubernetes driven FRRConfiguration be based on.
	// +kubebuilder:validation:Required
	FrrConfigurationSelector metav1.LabelSelector `json:"frrConfigurationSelector,omitempty"`

	// Advertisements determines what is advertised.
	// +kubebuilder:validation:Required
	Advertisements Advertisements `json:"advertisements,omitempty"`
}

// Advertisements determines what is advertised.
// +kubebuilder:validation:XValidation:rule="self.podNetwork || self.egressIP",message="Either pod network or egress IPs should be advertised"
type Advertisements struct {
	// PodNetwork determines if the pod network routes should be advertised.
	// +kubebuilder:validation:Optional
	PodNetwork bool `json:"podNetwork,omitempty"`

	// PodNetwork determines if the network EgressIPs should be advertised.
	// +kubebuilder:validation:Optional
	EgressIP bool `json:"egressIP,omitempty"`
}

// RouteAdvertisementsStatus defines the observed state of RouteAdvertisements.
// It should always be reconstructable from the state of the cluster and/or
// outside world.
type RouteAdvertisementsStatus struct {
	// A concise indication of whether the RouteAdvertisements resource is
	// applied with success.
	// +kubebuilder:validation:Optional
	Status string `json:"status,omitempty"`

	// An array of condition objects indicating details about status of
	// RouteAdvertisements object.
	// +kubebuilder:validation:Optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// RouteAdvertisementsList contains a list of RouteAdvertisements
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type RouteAdvertisementsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RouteAdvertisements `json:"items"`
}
