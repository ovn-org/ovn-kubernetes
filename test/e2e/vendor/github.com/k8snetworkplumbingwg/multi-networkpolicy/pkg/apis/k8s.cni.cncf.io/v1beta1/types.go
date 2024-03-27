/*
Copyright 2020 The Kubernetes Authors.

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

package v1beta1

import (
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// +genclient
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +resourceName=multi-networkpolicies

// MultiNetworkPolicy ...
type MultiNetworkPolicy struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec MultiNetworkPolicySpec `json:"spec,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MultiNetworkPolicyList ...
type MultiNetworkPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MultiNetworkPolicy `json:"items"`
}

// MultiPolicyType ...
type MultiPolicyType string

const (
	// PolicyTypeIngress ...
	PolicyTypeIngress MultiPolicyType = "Ingress"
	// PolicyTypeEgress ...
	PolicyTypeEgress MultiPolicyType = "Egress"
)

// MultiNetworkPolicySpec ...
type MultiNetworkPolicySpec struct {
	PodSelector metav1.LabelSelector `json:"podSelector"`

	// +optional
	Ingress []MultiNetworkPolicyIngressRule `json:"ingress,omitempty"`

	// +optional
	Egress []MultiNetworkPolicyEgressRule `json:"egress,omitempty"`
	// +optional
	PolicyTypes []MultiPolicyType `json:"policyTypes,omitempty"`
}

// MultiNetworkPolicyIngressRule ...
type MultiNetworkPolicyIngressRule struct {
	// +optional
	Ports []MultiNetworkPolicyPort `json:"ports,omitempty"`

	// +optional
	From []MultiNetworkPolicyPeer `json:"from,omitempty"`
}

// MultiNetworkPolicyEgressRule ...
type MultiNetworkPolicyEgressRule struct {
	// +optional
	Ports []MultiNetworkPolicyPort `json:"ports,omitempty"`

	// +optional
	To []MultiNetworkPolicyPeer `json:"to,omitempty"`
}

// MultiNetworkPolicyPort ...
type MultiNetworkPolicyPort struct {
	// +optional
	Protocol *v1.Protocol `json:"protocol,omitempty"`

	// +optional
	Port *intstr.IntOrString `json:"port,omitempty"`
}

// IPBlock ...
type IPBlock struct {
	CIDR string `json:"cidr"`
	// +optional
	Except []string `json:"except,omitempty"`
}

// MultiNetworkPolicyPeer ...
type MultiNetworkPolicyPeer struct {
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// +optional
	IPBlock *IPBlock `json:"ipBlock,omitempty"`
}
