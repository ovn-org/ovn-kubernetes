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
// +kubebuilder:resource:path=userdefinednetworks,scope=Namespaced
// +kubebuilder:singular=userdefinednetwork
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// UserDefinedNetwork describe network request for a Namespace.
type UserDefinedNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Spec is immutable"
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Layer3' ? has(self.layer3): !has(self.layer3)", message="spec.layer3 is required when topology is Layer3 and forbidden otherwise"
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'Layer2' ? has(self.layer2): !has(self.layer2)", message="spec.layer2 is required when topology is Layer2 and forbidden otherwise"
	// +kubebuilder:validation:XValidation:rule="has(self.topology) && self.topology == 'LocalNet' ? has(self.localNet): !has(self.localNet)", message="spec.localNet is required when topology is LocalNet and forbidden otherwise"
	// +required
	Spec UserDefinedNetworkSpec `json:"spec"`
	// +optional
	Status UserDefinedNetworkStatus `json:"status,omitempty"`
}

// UserDefinedNetworkSpec defines the desired state of UserDefinedNetworkSpec.
// +union
type UserDefinedNetworkSpec struct {
	// topology describes network configuration.
	// Allowed values are "Layer3", "Layer2", "LocalNet".
	// Layer3 topology creates a layer 2 segment per node, each with a different subnet. Layer 3 routing is used to interconnect node subnets.
	// Layer2 topology creates one logical switch shared by all nodes.
	// LocalNet topology creates a cluster-wide logical switch connected to a physical network.
	// +kubebuilder:validation:Required
	// +unionDiscriminator
	// +required
	Topology NetworkTopology `json:"topology"`

	// l3Config is the Layer3 configuration.
	// +optional
	Layer3 *L3Config `json:"layer3,omitempty"`

	// l2Config is the Layer2 configuration.
	// +optional
	Layer2 *L2Config `json:"layer2,omitempty"`

	// localNetConfig is the LocalNet configuration.
	// +optional
	LocalNet *LocalNetConfig `json:"localNet,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.subnets) && size(self.subnets) > 0", message="Subnets is required for Layer3 topology"
// +kubebuilder:validation:XValidation:rule="!has(self.joinSubnets) || has(self.role) && self.role == 'Primary'", message="JoinSubnets is only supported for Primary network"
type L3Config struct {
	// role describes the network role in the pod.
	// Allowed values are "Primary" and "Secondary".
	// Primary network is automatically assigned to every pod created in the same namespace.
	// Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network.
	// +kubebuilder:validation:Required
	// +required
	Role NetworkRole `json:"role"`

	// mtu is the maximum transmission unit for a network.
	// MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65536
	// +optional
	MTU int32 `json:"mtu,omitempty"`

	// subnets are used for the pod network across the cluster.
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// Given subnet is split into smaller subnets for every node.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="size(self) != 2 || isCIDR(self[0].cidr) && isCIDR(self[1].cidr) && cidr(self[0].cidr).ip().family() != cidr(self[1].cidr).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
	// +required
	Subnets []L3Subnet `json:"subnets,omitempty"`

	// joinSubnets are used inside the OVN network topology.
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// This field is only allowed for "Primary" network.
	// It is not recommended to set this field without explicit need and understanding of the OVN network topology.
	// When omitted, the platform will choose a reasonable default which is subject to change over time.
	// +optional
	JoinSubnets DualStackCIDRs `json:"joinSubnets,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.hostSubnet) || (isCIDR(self.cidr) && self.hostSubnet > cidr(self.cidr).prefixLength())", message="hostSubnet must be smaller than CIDR subnet"
// +kubebuilder:validation:XValidation:rule="!has(self.hostSubnet) || (isCIDR(self.cidr) && (cidr(self.cidr).ip().family() == 6 || self.hostSubnet < 32))", message="hostSubnet must < 32 for ipv4 CIDR"
type L3Subnet struct {
	// cidr specifies L3Subnet, which is split into smaller subnets for every node.
	// +required
	CIDR CIDR `json:"cidr,omitempty"`
	// hostSubnet specifies the subnet size for every node.
	// When not set, it will be assigned automatically.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=127
	// +optional
	HostSubnet int32 `json:"hostSubnet,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.role != 'Primary' || has(self.subnets) && size(self.subnets) > 0", message="Subnets is required for Primary Layer2 topology"
// +kubebuilder:validation:XValidation:rule="!has(self.joinSubnets) || has(self.role) && self.role == 'Primary'", message="JoinSubnets is only supported for Primary network"
// +kubebuilder:validation:XValidation:rule="!has(self.ipamLifecycle) || has(self.subnets) && size(self.subnets) > 0", message="IPAMLifecycle is only supported when subnets are set"
type L2Config struct {
	// role describes the network role in the pod.
	// Allowed value is "Secondary".
	// Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network.
	// +kubebuilder:validation:Required
	// +required
	Role NetworkRole `json:"role"`

	// mtu is the maximum transmission unit for a network.
	// MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65536
	// +optional
	MTU int32 `json:"mtu,omitempty"`

	// subnets are used for the pod network across the cluster.
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	//
	// The format should match standard CIDR notation (for example, "10.128.0.0/16").
	// This field may be omitted. In that case the logical switch implementing the network only provides layer 2 communication,
	// and users must configure IP addresses for the pods. As a consequence, Port security only prevents MAC spoofing.
	// +optional
	Subnets DualStackCIDRs `json:"subnets,omitempty"`

	// joinSubnets are used inside the OVN network topology.
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// This field is only allowed for "Primary" network.
	// It is not recommended to set this field without explicit need and understanding of the OVN network topology.
	// When omitted, the platform will choose a reasonable default which is subject to change over time.
	// +optional
	JoinSubnets DualStackCIDRs `json:"joinSubnets,omitempty"`

	// ipamLifecycle controls IP addresses management lifecycle.
	// The only allowed value is Persistent. When set, OVN Kubernetes assigned IP addresses will be persisted in an
	// `ipamclaims.k8s.cni.cncf.io` object. These IP addresses will be reused by other pods if requested.
	// Only supported when "subnets" are set.
	// +optional
	IPAMLifecycle NetworkIPAMLifecycle `json:"ipamLifecycle,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.role == 'Secondary'", message="LocalNet topology is only supported for Secondary network"
// +kubebuilder:validation:XValidation:rule="!has(self.excludeSubnets) || has(self.subnets) && size(self.subnets) > 0", message="ExcludeSubnets is only supported when Subnets is provided"
// +kubebuilder:validation:XValidation:rule="!has(self.ipamLifecycle) || has(self.subnets) && size(self.subnets) > 0", message="IPAMLifecycle is only supported when subnets are set"
type LocalNetConfig struct {
	// role describes the network role in the pod.
	// Allowed values are "Primary" and "Secondary".
	// Must be set to "Secondary".
	// +kubebuilder:validation:Required
	// +required
	Role NetworkRole `json:"role"`

	// mtu is the maximum transmission unit for a network.
	// MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65536
	// +optional
	MTU int32 `json:"mtu,omitempty"`

	// subnets are used for the pod network across the cluster.
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.

	// The format should match standard CIDR notation <example>.
	// This field may be omitted.
	// In that case the logical switch implementing the network only provides layer 2 communication,
	// and users must configure IP addresses for the pods. As a consequence, Port security only prevents MAC spoofing.
	// +optional
	Subnets DualStackCIDRs `json:"subnets,omitempty"`

	// excludeSubnets is a list of CIDRs that will be removed from the assignable IP address pool specified by the "subnets" field.
	// Should only be passed if "subnets" are set.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +optional
	ExcludeSubnets []CIDR `json:"excludeSubnets,omitempty"`

	// ipamLifecycle controls IP addresses management lifecycle.
	// The only allowed value is Persistent. When set, OVN Kubernetes assigned IP addresses will be persisted in an
	// `ipamclaims.k8s.cni.cncf.io` object. These IP addresses will be reused by other pods if requested.
	// Only supported when "subnets" are set.
	// +optional
	IPAMLifecycle NetworkIPAMLifecycle `json:"ipamLifecycle,omitempty"`
}

// +kubebuilder:validation:Enum=Layer2;Layer3;LocalNet
type NetworkTopology string

const (
	NetworkTopologyLayer2   NetworkTopology = "Layer2"
	NetworkTopologyLayer3   NetworkTopology = "Layer3"
	NetworkTopologyLocalNet NetworkTopology = "LocalNet"
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

// +kubebuilder:validation:XValidation:rule="isCIDR(self)", message="CIDR is invalid"
type CIDR string

// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=2
// +kubebuilder:validation:XValidation:rule="size(self) != 2 || isCIDR(self[0]) && isCIDR(self[1]) && cidr(self[0]).ip().family() != cidr(self[1]).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
type DualStackCIDRs []CIDR
