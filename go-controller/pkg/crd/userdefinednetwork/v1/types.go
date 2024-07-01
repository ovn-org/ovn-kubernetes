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
// +kubebuilder:resource:path=userdefinednetworks,scope=Namespaced,shortName=udn
// +kubebuilder:singular=userdefinednetwork
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// UserDefinedNetwork describe network request for a Namespace.
type UserDefinedNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Spec is immutable"
	// +kubebuilder:validation:XValidation:rule="self.topology != 'Layer3'   || (self.topology == 'Layer3' && size(self.subnets) > 0)", message="Subnets is required for Layer3 topology"
	// +kubebuilder:validation:XValidation:rule="self.topology != 'Localnet' || (self.topology == 'Localnet' && self.role == 'Secondary')", message="Localnet topology is not supported for primary network"
	// +kubebuilder:validation:XValidation:rule="!has(self.ipamLifecycle)    || (self.ipamLifecycle == 'Persistent' && (self.topology == 'Layer2' || self.topology == 'Localnet'))", message="IPAMLifecycle is supported for Layer2 and Localnet topologies"
	// +required
	Spec UserDefinedNetworkSpec `json:"spec"`
	// +optional
	Status UserDefinedNetworkStatus `json:"status,omitempty"`
}

// UserDefinedNetworkSpec defines the desired state of UserDefinedNetworkSpec.
type UserDefinedNetworkSpec struct {
	// The topological configuration for the network.
	// +kubebuilder:validation:Required
	// +required
	Topology NetworkTopology `json:"topology"`

	// The network role in the pod (e.g.: Primary, Secondary).
	// +kubebuilder:validation:Required
	// +required
	Role NetworkRole `json:"role"`

	// The maximum transmission unit (MTU).
	// MTU is optional, if not provided the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network.
	// +optional
	MTU uint `json:"mtu,omitempty"`

	// The subnet to use for the pod network across the cluster.
	//
	// Dual-stack clusters may set 2 subnets (one for each IP family),
	// otherwise only 1 subnet is allowed.
	//
	// When topology is `Layer3`, the given subnet is split into smaller subnets for every node.
	// To specify how the subnet should be split, the following format is supported for `Layer3` network:
	// `10.128.0.0/16/24`, which means that every host will get a `/24` subnet.
	// If host subnet mask is not set (for example, `10.128.0.0/16`), it will be assigned automatically.
	//
	// For `Layer2` and `Localnet` topology types, the format should match standard CIDR notation, without
	// providing any host subnet mask.
	// This field may be omitted for `Layer2` and `Localnet` topologies.
	// In that case the logical switch implementing the network only provides layer 2 communication,
	// and users must configure IP addresses for the pods.
	// Port security only prevents MAC spoofing
	// +optional
	Subnets []string `json:"subnets,omitempty"`

	// A list of CIDRs.
	// IP addresses are removed from the assignable IP address pool and are never passed to the pods.
	// +optional
	ExcludeSubnets []string `json:"excludeSubnets,omitempty"`

	// Subnet used inside the OVN network topology.
	// This field is ignored for non-primary networks (e.g.: Role Secondary).
	// When omitted, this means no opinion and the platform is left to choose a reasonable default which is subject to change over time.
	// +kubebuilder:validation:XValidation:rule="1 <= size(self) && size(self) <= 2", message="Unexpected number of join subnets"
	// +optional
	JoinSubnets []string `json:"joinSubnets,omitempty"`

	// Control IP addresses management lifecycle.
	// When `Persistent` is specified it enable workloads have persistent IP addresses.
	// For example: Virtual Machines will have the same IP addresses along their lifecycle (stop, start migration, reboots).
	// Supported by Topology `Layer2` and `Localnet`.
	// +optional
	IPAMLifecycle NetworkIPAMLifecycle `json:"ipamLifecycle,omitempty"`
}

// +kubebuilder:validation:Enum=Layer2;Layer3;Localnet
type NetworkTopology string

const (
	NetworkTopologyLayer2   NetworkTopology = "Layer2"
	NetworkTopologyLayer3   NetworkTopology = "Layer3"
	NetworkTopologyLocalnet NetworkTopology = "Localnet"
)

// +kubebuilder:validation:Enum=Primary;Secondary
type NetworkRole string

const (
	NetworkRolePrimary   NetworkRole = "Primary"
	NetworkRoleSecondary NetworkRole = "Secondary"
)

// +kubebuilder:validation:Enum=Persistent
type NetworkIPAMLifecycle string

const IPAMLifecyclePersistent NetworkIPAMLifecycle = "Persistent"

// UserDefinedNetworkList contains a list of UserDefinedNetwork.
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
