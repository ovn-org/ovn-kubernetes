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
// +kubebuilder:resource:path=podnetworks
// +kubebuilder::singular=podnetwork
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="PodName",type=string,JSONPath=`.spec.podName`
// +kubebuilder:printcolumn:name="NodeName",type=string,JSONPath=`.spec.nodeName`
// +noStatus
type PodNetwork struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +optional
	Spec PodNetworkSpec `json:"spec,omitempty"`
}

type PodNetworkSpec struct {
	// PodName is used for better visibility of related pod (since PodNetwork.Name = string(pod.UID))
	PodName string `json:"podName"`
	// NodeName is a name of the node where owner pod resides.
	NodeName string                `json:"nodeName"`
	Networks map[string]OVNNetwork `json:"networks,omitempty"`
}

type OVNNetwork struct {
	IPs []string `json:"ip_addresses"`
	MAC string   `json:"mac_address"`
	// +optional
	Gateways []string `json:"gateway_ips,omitempty"`
	// +optional
	Routes []PodRoute `json:"routes,omitempty"`
}

// Internal struct used to marshal PodRoute to the pod annotation
type PodRoute struct {
	Dest    string `json:"dest"`
	NextHop string `json:"nextHop"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=podnetworks
type PodNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodNetwork `json:"items"`
}
