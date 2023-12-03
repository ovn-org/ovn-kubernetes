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

// AdminPolicyBasedExternalRoute is a CRD allowing the cluster administrators to configure policies for external gateway IPs to be applied to all the pods contained in selected namespaces.
// Egress traffic from the pods that belong to the selected namespaces to outside the cluster is routed through these external gateway IPs.
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=adminpolicybasedexternalroutes,scope=Cluster,shortName=apbexternalroute,singular=adminpolicybasedexternalroute
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Last Update",type="date",JSONPath=`.status.lastTransitionTime`
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=`.status.status`
type AdminPolicyBasedExternalRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +required
	Spec AdminPolicyBasedExternalRouteSpec `json:"spec"`
	// +optional
	Status AdminPolicyBasedRouteStatus `json:"status,omitempty"`
}

// AdminPolicyBasedExternalRouteSpec defines the desired state of AdminPolicyBasedExternalRoute
type AdminPolicyBasedExternalRouteSpec struct {
	// From defines the selectors that will determine the target namespaces to this CR.
	From ExternalNetworkSource `json:"from"`
	// NextHops defines two types of hops: Static and Dynamic. Each hop defines at least one external gateway IP.
	NextHops ExternalNextHops `json:"nextHops"`
}

// ExternalNetworkSource contains the selectors used to determine the namespaces where the policy will be applied to
type ExternalNetworkSource struct {
	// NamespaceSelector defines a selector to be used to determine which namespaces will be targeted by this CR
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`
}

// +kubebuilder:validation:MinProperties:=1
// ExternalNextHops contains slices of StaticHops and DynamicHops structures. Minimum is one StaticHop or one DynamicHop.
type ExternalNextHops struct {
	// StaticHops defines a slice of StaticHop. This field is optional.
	StaticHops []*StaticHop `json:"static,omitempty"`
	//DynamicHops defines a slices of DynamicHop. This field is optional.
	DynamicHops []*DynamicHop `json:"dynamic,omitempty"`
}

// StaticHop defines the configuration of a static IP that acts as an external Gateway Interface. IP field is mandatory.
type StaticHop struct {
	//IP defines the static IP to be used for egress traffic. The IP can be either IPv4 or IPv6.
	// + Regex taken from: https://blog.markhatton.co.uk/2011/03/15/regular-expressions-for-ip-addresses-cidr-ranges-and-hostnames/
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])\.){3}([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])$|^s*((([0-9A-Fa-f]{1,4}:){7}([0-9A-Fa-f]{1,4}|:))|(([0-9A-Fa-f]{1,4}:){6}(:[0-9A-Fa-f]{1,4}|((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3})|:))|(([0-9A-Fa-f]{1,4}:){5}(((:[0-9A-Fa-f]{1,4}){1,2})|:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3})|:))|(([0-9A-Fa-f]{1,4}:){4}(((:[0-9A-Fa-f]{1,4}){1,3})|((:[0-9A-Fa-f]{1,4})?:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:))|(([0-9A-Fa-f]{1,4}:){3}(((:[0-9A-Fa-f]{1,4}){1,4})|((:[0-9A-Fa-f]{1,4}){0,2}:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:))|(([0-9A-Fa-f]{1,4}:){2}(((:[0-9A-Fa-f]{1,4}){1,5})|((:[0-9A-Fa-f]{1,4}){0,3}:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:))|(([0-9A-Fa-f]{1,4}:){1}(((:[0-9A-Fa-f]{1,4}){1,6})|((:[0-9A-Fa-f]{1,4}){0,4}:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:))|(:(((:[0-9A-Fa-f]{1,4}){1,7})|((:[0-9A-Fa-f]{1,4}){0,5}:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:)))(%.+)?s*`
	// +required
	IP string `json:"ip"`
	// BFDEnabled determines if the interface implements the Bidirectional Forward Detection protocol. Defaults to false.
	// +optional
	// +kubebuilder:default:=false
	// +default=false
	BFDEnabled bool `json:"bfdEnabled,omitempty"`
	// SkipHostSNAT determines whether to disable Source NAT to the host IP. Defaults to false.
	// +optional
	// +kubebuilder:default:=false
	// +default=false
	// SkipHostSNAT bool `json:"skipHostSNAT,omitempty"`
}

// DynamicHop defines the configuration for a dynamic external gateway interface.
// These interfaces are wrapped around a pod object that resides inside the cluster.
// The field NetworkAttachmentName captures the name of the multus network name to use when retrieving the gateway IP to use.
// The PodSelector and the NamespaceSelector are mandatory fields.
type DynamicHop struct {
	// PodSelector defines the selector to filter the pods that are external gateways.
	// +kubebuilder:validation:Required
	// +required
	PodSelector metav1.LabelSelector `json:"podSelector"`
	// NamespaceSelector defines a selector to filter the namespaces where the pod gateways are located.
	// +kubebuilder:validation:Required
	// +required
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`
	// NetworkAttachmentName determines the multus network name to use when retrieving the pod IPs that will be used as the gateway IP.
	// When this field is empty, the logic assumes that the pod is configured with HostNetwork and is using the node's IP as gateway.
	// +optional
	// +kubebuilder:default=""
	// +default=""
	NetworkAttachmentName string `json:"networkAttachmentName,omitempty"`
	// BFDEnabled determines if the interface implements the Bidirectional Forward Detection protocol. Defaults to false.
	// +optional
	// +kubebuilder:default:=false
	// +default=false
	BFDEnabled bool `json:"bfdEnabled,omitempty"`
	// SkipHostSNAT determines whether to disable Source NAT to the host IP. Defaults to false
	// +optional
	// +kubebuilder:default:=false
	// +default=false
	// SkipHostSNAT bool `json:"skipHostSNAT,omitempty"`
}

// AdminPolicyBasedExternalRouteList contains a list of AdminPolicyBasedExternalRoutes
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AdminPolicyBasedExternalRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AdminPolicyBasedExternalRoute `json:"items"`
}

// AdminPolicyBasedRouteStatus contains the observed status of the AdminPolicyBased route types.
type AdminPolicyBasedRouteStatus struct {
	// Captures the time when the last change was applied.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	// An array of Human-readable messages indicating details about the status of the object.
	// +patchStrategy=merge
	// +listType=set
	// +optional
	Messages []string `json:"messages,omitempty"`
	// A concise indication of whether the AdminPolicyBasedRoute resource is applied with success
	// +optional
	Status StatusType `json:"status,omitempty"`
}

// StatusType defines the types of status used in the Status field. The value determines if the
// deployment of the CR was successful or if it failed.
type StatusType string

const (
	SuccessStatus StatusType = "Success"
	FailStatus    StatusType = "Fail"
)
