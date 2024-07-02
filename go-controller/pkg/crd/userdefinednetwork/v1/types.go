/*
Copyright 2023.

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
// +kubebuilder:resource:path=userdefinednetworks
// +kubebuilder:scope=Namespaced
// +kubebuilder:shortName=udn
// +kubebuilder:singular=userdefinednetwork
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// UserDefinedNetwork describe network request for a Namespace
type UserDefinedNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +required
	Spec UserDefinedNetworkSpec `json:"spec"`
	// +optional
	Status UserDefinedNetworkStatus `json:"status,omitempty"`
}

// UserDefinedNetworkSpec defines the desired state of UserDefinedNetworkSpec
type UserDefinedNetworkSpec struct {
	Foo string `json:"foo"`
}

// UserDefinedNetworkList contains a list of UserDefinedNetwork
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type UserDefinedNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UserDefinedNetwork `json:"items"`
}

// UserDefinedNetworkStatus contains the observed status of the UserDefinedNetwork.
type UserDefinedNetworkStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
