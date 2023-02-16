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
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=adminpolicybasedexternalroute,scope=Cluster
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// AdminPolicyBasedExternalRoute is the Schema for the AdminPolicyBasedExternalRoutes API
type AdminPolicyBasedExternalRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AdminPolicyBasedExternalRouteSpec `json:"spec,omitempty"`
	Status            AdminPolicyBasedRouteStatus       `json:"status,omitempty"`
}

// AdminPolicyBasedExternalRouteSpec defines the desired state of AdminPolicyBasedExternalRoute
type AdminPolicyBasedExternalRouteSpec struct {
	Policies []*ExternalPolicy `json:"policies"`
}

type ExternalPolicy struct {
	From     ExternalNetworkSource `json:"from"`
	NextHops ExternalNextHops      `json:"nexthops"`
}

type ExternalNetworkSource struct {
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

type ExternalNextHops struct {
	// +kubebuilder:validation:UniqueItems=true
	StaticHops []*StaticHop `json:"static,omitempty"`
	// +kubebuilder:validation:UniqueItems=true
	DynamicHops []*DynamicHop `json:"dynamic,omitempty"`
}

type StaticHop struct {
	IP           string `json:"ip,omitempty"`
	BFDEnabled   bool   `json:"bfdEnabled,omitempty"`
	SkipHostSNAT bool   `json:"skipHostSNAT,omitempty"`
}

type DynamicHop struct {
	PodSelector           metav1.LabelSelector  `json:"podSelector,omitempty"`
	NamespaceSelector     *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
	NetworkAttachmentName string                `json:"networkAttachmentName,omitempty"`
	BFDEnabled            bool                  `json:"bfdEnabled,omitempty"`
	SkipHostSNAT          bool                  `json:"skipHostSNAT,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// AdminPolicyBasedExternalRouteList contains a list of AdminPolicyBasedExternalRoutes
type AdminPolicyBasedExternalRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AdminPolicyBasedExternalRoute `json:"items"`
}

// AdminPolicyBasedRouteStatus describes the current status of the AdminPolicyBased route types.
// +kubebuilder:object:root=true
type AdminPolicyBasedRouteStatus struct {
	// An array of Human-readable messages indicating details about the status of the object.
	Messages []string `json:"messages,omitempty"`
	// A concise indication of whether the AdminPolicyBasedRoute resource is applied or not
	Status StatusType `json:"status,omitempty"`
}

type StatusType string

const (
	SuccessStatus StatusType = "Success"
	FailStatus    StatusType = "Fail"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=adminpolicybasedexternalroute,scope=Cluster
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type AdminPolicyBasedInternalRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AdminPolicyBasedInternalRouteSpec `json:"spec,omitempty"`
	Status            AdminPolicyBasedRouteStatus       `json:"status,omitempty"`
}

// AdminPolicyBasedInternalRouteSpec defines the desired state of AdminPolicyBasedInternalRoute
type AdminPolicyBasedInternalRouteSpec struct {
	Policies []*InternalPolicies `json:"policies"`
}

type InternalPolicies struct {
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// AdminPolicyBasedExternalRouteList contains a list of AdminPolicyBasedExternalRoutes
type AdminPolicyBasedInternalRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AdminPolicyBasedInternalRoute `json:"items"`
}
