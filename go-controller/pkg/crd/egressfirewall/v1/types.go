package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EgressNetworkFirewallRuleType indicates whether an EgressNetworkFirewallRule allows or denies traffic
// +kubebuilder:validation:Pattern=^Allow|Deny$
type EgressFirewallRuleType string

const (
	EgressFirewallRuleAllow EgressFirewallRuleType = "Allow"
	EgressFirewallRuleDeny  EgressFirewallRuleType = "Deny"
)

// +genclient
// +resource:path=egressfirewall
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:printcolumn:name="EgressFirewall Status",type=string,JSONPath=".status.status"
// EgressFirewall describes the current egress firewall for a Namespace.
// Traffic from a pod to an IP address outside the cluster will be checked against
// each EgressFirewallRule in the pod's namespace's EgressFirewall, in
// order. If no rule matches (or no EgressFirewall is present) then the traffic
// will be allowed by default.
type EgressFirewall struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Specification of the desired behavior of EgressFirewall.
	Spec EgressFirewallSpec `json:"spec"`
	// Observed status of EgressFirewall
	// +optional
	Status EgressFirewallStatus `json:"status,omitempty"`
}

type EgressFirewallStatus struct {
	Status string `json:"status,omitempty"`
}

// EgressFirewallSpec is a desired state description of EgressFirewall.
type EgressFirewallSpec struct {
	// a collection of egress firewall rule objects
	Egress []EgressFirewallRule `json:"egress"`
}

// EgressFirewallRule is a single egressfirewall rule object
type EgressFirewallRule struct {
	// type marks this as an "Allow" or "Deny" rule
	Type EgressFirewallRuleType `json:"type"`
	// ports specify what ports and protocols the rule applies to
	// +optional
	Ports []EgressFirewallPort `json:"ports,omitempty"`
	// to is the target that traffic is allowed/denied to
	To EgressFirewallDestination `json:"to"`
}

// EgressFirewallPort specifies the port to allow or deny traffic to
type EgressFirewallPort struct {
	// protocol (tcp, udp, sctp) that the traffic must match.
	// +kubebuilder:validation:Pattern=^TCP|UDP|SCTP$
	Protocol string `json:"protocol"`
	// port that the traffic must match
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=65535
	Port int32 `json:"port"`
}

// EgressFirewallDestination is the endpoint that traffic is either allowed or denied to
type EgressFirewallDestination struct {
	// cidrSelector is the CIDR range to allow/deny traffic to. If this is set, dnsName must be unset.
	CIDRSelector string `json:"cidrSelector,omitempty"`
	// dnsName is the domain name to allow/deny traffic to. If this is set, cidrSelector must be unset.
	// +kubebuilder:validation:Pattern=^([A-Za-z0-9-]+\.)*[A-Za-z0-9-]+\.?$
	DNSName string `json:"dnsName,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +resource:path=egressfirewall
// EgressFirewallList is the list of EgressFirewalls.
type EgressFirewallList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	// List of EgressFirewalls.
	Items []EgressFirewall `json:"items"`
}
