/*
Copyright 2024.

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

// +kubebuilder:validation:Enum=Layer2;Layer3
type NetworkTopology string

const (
	NetworkTopologyLayer2 NetworkTopology = "Layer2"
	NetworkTopologyLayer3 NetworkTopology = "Layer3"
)

// +kubebuilder:validation:XValidation:rule="!has(self.joinSubnets) || has(self.role) && self.role == 'Primary'", message="JoinSubnets is only supported for Primary network"
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || !has(self.mtu) || !self.subnets.exists_one(i, isCIDR(i.cidr) && cidr(i.cidr).ip().family() == 6) || self.mtu >= 1280", message="MTU should be greater than or equal to 1280 when IPv6 subent is used"
type Layer3Config struct {
	// Role describes the network role in the pod.
	//
	// Allowed values are "Primary" and "Secondary".
	// Primary network is automatically assigned to every pod created in the same namespace.
	// Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network.
	//
	// +kubebuilder:validation:Required
	// +required
	Role NetworkRole `json:"role"`

	// MTU is the maximum transmission unit for a network.
	//
	// MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network.
	//
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=65536
	// +optional
	MTU int32 `json:"mtu,omitempty"`

	// Subnets are used for the pod network across the cluster.
	//
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// Given subnet is split into smaller subnets for every node.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +required
	// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0].cidr) || !isCIDR(self[1].cidr) || cidr(self[0].cidr).ip().family() != cidr(self[1].cidr).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
	Subnets []Layer3Subnet `json:"subnets,omitempty"`

	// JoinSubnets are used inside the OVN network topology.
	//
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// This field is only allowed for "Primary" network.
	// It is not recommended to set this field without explicit need and understanding of the OVN network topology.
	// When omitted, the platform will choose a reasonable default which is subject to change over time.
	//
	// +optional
	JoinSubnets DualStackCIDRs `json:"joinSubnets,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.hostSubnet) || !isCIDR(self.cidr) || self.hostSubnet > cidr(self.cidr).prefixLength()", message="HostSubnet must be smaller than CIDR subnet"
// +kubebuilder:validation:XValidation:rule="!has(self.hostSubnet) || !isCIDR(self.cidr) || (cidr(self.cidr).ip().family() == 4 && self.hostSubnet < 32)", message="HostSubnet must < 32 for ipv4 CIDR"
type Layer3Subnet struct {
	// CIDR specifies L3Subnet, which is split into smaller subnets for every node.
	//
	// +required
	CIDR CIDR `json:"cidr,omitempty"`

	// HostSubnet specifies the subnet size for every node.
	//
	// When not set, it will be assigned automatically.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=127
	// +optional
	HostSubnet int32 `json:"hostSubnet,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.ipam) && has(self.ipam.mode) && self.ipam.mode != 'Enabled' || has(self.subnets)", message="Subnets is required with ipam.mode is Enabled or unset"
// +kubebuilder:validation:XValidation:rule="!has(self.ipam) || !has(self.ipam.mode) || self.ipam.mode != 'Disabled' || !has(self.subnets)", message="Subnets must be unset when ipam.mode is Disabled"
// +kubebuilder:validation:XValidation:rule="!has(self.ipam) || !has(self.ipam.mode) || self.ipam.mode != 'Disabled' || self.role == 'Secondary'", message="Disabled ipam.mode is only supported for Secondary network"
// +kubebuilder:validation:XValidation:rule="!has(self.joinSubnets) || has(self.role) && self.role == 'Primary'", message="JoinSubnets is only supported for Primary network"
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || !has(self.mtu) || !self.subnets.exists_one(i, isCIDR(i) && cidr(i).ip().family() == 6) || self.mtu >= 1280", message="MTU should be greater than or equal to 1280 when IPv6 subent is used"
type Layer2Config struct {
	// Role describes the network role in the pod.
	//
	// Allowed value is "Secondary".
	// Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network.
	//
	// +kubebuilder:validation:Required
	// +required
	Role NetworkRole `json:"role"`

	// MTU is the maximum transmission unit for a network.
	// MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network.
	//
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=65536
	// +optional
	MTU int32 `json:"mtu,omitempty"`

	// Subnets are used for the pod network across the cluster.
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	//
	// The format should match standard CIDR notation (for example, "10.128.0.0/16").
	// This field must be omitted if `ipam.mode` is `Disabled`.
	//
	// +optional
	Subnets DualStackCIDRs `json:"subnets,omitempty"`

	// JoinSubnets are used inside the OVN network topology.
	//
	// Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.
	// This field is only allowed for "Primary" network.
	// It is not recommended to set this field without explicit need and understanding of the OVN network topology.
	// When omitted, the platform will choose a reasonable default which is subject to change over time.
	//
	// +optional
	JoinSubnets DualStackCIDRs `json:"joinSubnets,omitempty"`

	// IPAM section contains IPAM-related configuration for the network.
	// +optional
	IPAM *IPAMConfig `json:"ipam,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.lifecycle) || self.lifecycle != 'Persistent' || !has(self.mode) || self.mode == 'Enabled'", message="lifecycle Persistent is only supported when ipam.mode is Enabled"
// +kubebuilder:validation:MinProperties=1
type IPAMConfig struct {
	// Mode controls how much of the IP configuration will be managed by OVN.
	// `Enabled` means OVN-Kubernetes will apply IP configuration to the SDN infrastructure and it will also assign IPs
	// from the selected subnet to the individual pods.
	// `Disabled` means OVN-Kubernetes will only assign MAC addresses and provide layer 2 communication, letting users
	// configure IP addresses for the pods.
	// `Disabled` is only available for Secondary networks.
	// By disabling IPAM, any Kubernetes features that rely on selecting pods by IP will no longer function
	// (such as network policy, services, etc). Additionally, IP port security will also be disabled for interfaces attached to this network.
	// Defaults to `Enabled`.
	// +optional
	Mode IPAMMode `json:"mode,omitempty"`

	// Lifecycle controls IP addresses management lifecycle.
	//
	// The only allowed value is Persistent. When set, OVN Kubernetes assigned IP addresses will be persisted in an
	// `ipamclaims.k8s.cni.cncf.io` object. These IP addresses will be reused by other pods if requested.
	// Only supported when mode is `Enabled`.
	//
	// +optional
	Lifecycle NetworkIPAMLifecycle `json:"lifecycle,omitempty"`
}

// +kubebuilder:validation:Enum=Enabled;Disabled
type IPAMMode string

const (
	IPAMEnabled  IPAMMode = "Enabled"
	IPAMDisabled IPAMMode = "Disabled"
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

// +kubebuilder:validation:XValidation:rule="isCIDR(self)", message="CIDR is invalid"
// +kubebuilder:validation:MaxLength=43
type CIDR string

// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=2
// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0]) || !isCIDR(self[1]) || cidr(self[0]).ip().family() != cidr(self[1]).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
type DualStackCIDRs []CIDR
