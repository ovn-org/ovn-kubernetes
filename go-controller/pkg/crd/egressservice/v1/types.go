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
// +kubebuilder:resource:path=egressservices
// +kubebuilder::singular=egressservice
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// EgressService is a CRD that allows the user to request that the source
// IP of egress packets originating from all of the pods that are endpoints
// of a given LoadBalancer Service would be its ingress IP.
type EgressService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EgressServiceSpec   `json:"spec,omitempty"`
	Status EgressServiceStatus `json:"status,omitempty"`
}

// EgressServiceSpec defines the desired state of EgressService
type EgressServiceSpec struct {
	// Allows limiting the nodes that can be selected to handle the service's traffic.
	// When present only a node whose labels match the specified selectors can be selected
	// for handling the service's traffic.
	// When it is not specified any node in the cluster can be chosen to manage the service's traffic.
	// +optional
	NodeSelector metav1.LabelSelector `json:"nodeSelector,omitempty"`

	// The network which this service should send egress and corresponding ingress replies to.
	// This is typically implemented as VRF mapping, representing a numeric id or string name
	// of a routing table which by omission uses the default host routing.
	// +optional
	Network string `json:"network,omitempty"`
}

// EgressServiceStatus defines the observed state of EgressService
type EgressServiceStatus struct {
	// The name of the node selected to handle the service's traffic.
	Host string `json:"host"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=egressservices
// +kubebuilder::singular=egressservice
// EgressServiceList contains a list of EgressServices
type EgressServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressService `json:"items"`
}
