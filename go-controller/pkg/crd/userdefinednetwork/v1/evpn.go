// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

/*
Copyright 2025.

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

// EVPNConfig contains configuration options for networks operating in EVPN mode.
// +kubebuilder:validation:XValidation:rule="has(self.macVRF) || has(self.ipVRF)", message="at least one of macVRF or ipVRF must be specified"
// +kubebuilder:validation:XValidation:rule="!has(self.macVRF) || !has(self.ipVRF) || self.macVRF.vni != self.ipVRF.vni", message="macVRF and ipVRF must use different VNIs"
type EVPNConfig struct {
	// VTEP is the name of the VTEP CR that defines VTEP IPs for EVPN.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +required
	VTEP string `json:"vtep"`

	// MACVRF contains the MAC-VRF configuration for Layer 2 EVPN.
	// This field is required for Layer2 topology and forbidden for Layer3 topology.
	// +optional
	MACVRF *VRFConfig `json:"macVRF,omitempty"`

	// IPVRF contains the IP-VRF configuration for Layer 3 EVPN.
	// This field is required for Layer3 topology and optional for Layer2 topology.
	// +optional
	IPVRF *VRFConfig `json:"ipVRF,omitempty"`
}

// validateRouteTarget in go-controller/pkg/clustermanager/routeadvertisements/validation.go
// rejects everything the MaxLength and XValidation markers on RouteTargetString
// reject, and more; a change to these markers must be made there too.

// RouteTargetString represents the 6-byte value of a BGP extended community route target (RFC 4360).
// BGP Extended Communities are 8 bytes total: 2-byte type field + 6-byte value field.
// This string encodes the 6-byte value, split between a global administrator (Autonomous System or IPv4) and a local administrator.
//
// When auto-generated, the local administrator is set to the VNI, creating a natural mapping
// between Route Targets and VXLAN network segments (e.g., "65000:100" for AS 65000 and VNI 100).
// When explicitly specified, the local administrator can be any value within the type constraints.
//
// FRR EVPN L3 Route-Targets use format (A.B.C.D:MN|EF:OPQR|GHJK:MN|*:OPQR|*:MN) where:
//   - EF:OPQR   = 2-byte AS (1-65535) : local administrator (4 bytes, 0-4294967295)
//   - GHJK:MN   = 4-byte AS (65536-4294967295) : local administrator (2 bytes, 0-65535)
//   - A.B.C.D:MN = IPv4 address (4 bytes) : local administrator (2 bytes, 0-65535)
//   - *:OPQR    = wildcard AS : local administrator (4 bytes, 0-4294967295) - for import matching
//   - *:MN      = wildcard AS : local administrator (2 bytes, 0-65535) - for import matching
//
// The 6-byte constraint means: if AS is 4 bytes, local admin can only be 2 bytes, and vice versa.
// Wildcard (*) matches any AS and is useful for import rules in Downstream VNI scenarios.
// Note: VNI is 24-bit (max 16777215), so auto-generation with 4-byte AS or IPv4 only works if VNI <= 65535.
// See: https://docs.frrouting.org/en/stable-8.5/bgp.html#evpn-l3-route-targets
//
// +kubebuilder:validation:MaxLength=21
// +kubebuilder:validation:XValidation:rule="self.split(':').size() == 2",message="RT must contain exactly one colon"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || (self.startsWith('*:') || isIP(self.split(':')[0]) || self.split(':')[0].matches('^[0-9]+$'))",message="RT global administrator must be either '*', an IPv4 address, or a number"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.split(':')[1].matches('^[0-9]+$')",message="RT local administrator must be a number"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || !self.startsWith('*:') || (self.split(':')[1].matches('^[0-9]+$') && uint(self.split(':')[1]) <= 4294967295u)",message="RT with wildcard global administrator must have format *:OPQR where OPQR <= 4294967295"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || !self.split(':')[0].contains('.') || (self.split(':')[1].matches('^[0-9]+$') && uint(self.split(':')[1]) <= 65535u)",message="RT with IPv4 global administrator must have format A.B.C.D:MN where MN <= 65535"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.startsWith('*:') || self.split(':')[0].contains('.') || !self.split(':')[0].matches('^[0-9]+$') || !self.split(':')[1].matches('^[0-9]+$') || uint(self.split(':')[0]) <= 65535u || uint(self.split(':')[1]) <= 65535u",message="RT with 4-byte ASN global administrator must have format GHJK:MN where GHJK <= 4294967295 and MN <= 65535"
// +kubebuilder:validation:XValidation:rule="self.split(':').size() != 2 || self.startsWith('*:') || self.split(':')[0].contains('.') || !self.split(':')[0].matches('^[0-9]+$') || !self.split(':')[1].matches('^[0-9]+$') || uint(self.split(':')[0]) > 65535u || uint(self.split(':')[1]) <= 4294967295u",message="RT with 2-byte ASN global administrator must have format EF:OPQR where EF <= 65535 and OPQR <= 4294967295"
type RouteTargetString string

// VRFConfig contains configuration for a VRF in EVPN.
type VRFConfig struct {
	// VNI is the Virtual Network Identifier for this VRF.
	// VNI is a 24-bit field in the VXLAN header (RFC 7348), allowing values from 1 to 16777215.
	// but in the future this could be having different limit for other dataplane implementations.
	// Must be unique across all EVPN configurations in the cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16777215
	// +required
	VNI int32 `json:"vni"`

	// RouteTarget is the import/export route target for this VRF.
	// If not specified, it will be auto-generated as "<AS (Autonomous System)>:<VNI (Virtual Network Identifier)>".
	// Auto-generation will use 2-byte AS if VNI > 65535, since 4-byte AS/IPv4 only allows 2-byte local admin.
	//
	// Follows FRR EVPN L3 Route-Target format (A.B.C.D:MN|EF:OPQR|GHJK:MN|*:OPQR|*:MN):
	//   - EF:OPQR   = 2-byte AS (1-65535) : local admin (4 bytes, 1-4294967295)
	//   - GHJK:MN   = 4-byte AS (65536-4294967295) : local admin (2 bytes, 1-65535)
	//   - A.B.C.D:MN = IPv4 address : local admin (2 bytes, 1-65535)
	//   - *:OPQR    = wildcard AS : local admin (4 bytes, 1-4294967295) - for import matching
	//   - *:MN      = wildcard AS : local admin (2 bytes, 1-65535) - for import matching
	//
	// The 6-byte value constraint (RFC 4360) means AS size + local admin size = 6 bytes.
	// +optional
	RouteTarget RouteTargetString `json:"routeTarget,omitempty"`
}
