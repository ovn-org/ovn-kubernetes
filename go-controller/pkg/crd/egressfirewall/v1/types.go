package v1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EgressNetworkFirewallRuleType indicates whether an EgressNetworkFirewallRule allows or denies traffic
type EgressFirewallRuleType string

const (
	EgressFirewallRuleAllow EgressFirewallRuleType = "Allow"
	EgressFirewallRuleDeny  EgressFirewallRuleType = "Deny"
)

// +genclient
// +resource:path=egressfirewall
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
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
	Status EgressFirewallStatus `json:"status"`
}

// EgressFirewallStatus describes the lifecycle status of EgressFirewall.
type EgressFirewallStatus struct {
}

// EgressFirewallSpec is a desired state description of EgressFirewall.
type EgressFirewallSpec struct {
	// a collection of egress firewall rule objects
	Rules []EgressFirewallRule `json:"rules"`
}

// EgressFirewallRule is a single egressfirewall rule object
type EgressFirewallRule struct {
	// type marks this as an "Allow" or "Deny" rule
	Type EgressFirewallRuleType `json:"type"`
	// ports mark what ports and protocols for the rule to affect
	Ports []EgressFirewallPort `json:"ports,omitempty"`
	// to is one or more targes that traffic is allowed/denied to
	To []EgressFirewallDestination `json:"to"`
}

// EgressFirewallPort specifies the port to allow or deny traffic to
type EgressFirewallPort struct {
	// protocol (TCP, UDP, or SCTP) that the traffic must match. Defaults to TCP
	// +optional
	Protocol *v1.Protocol `json:"protocol,omitempty"`
	// port that the traffic must match
	Port int32 `json:"port"`
}

// EgressFirewallDestination is the endpoint that traffic is either allowed or denied, can be either a dnsName or a cidrSelector
type EgressFirewallDestination struct {
	// dnsName is the domain name to allow/deny traffic to. If this is set, cidrSelector must be unset.
	DNSName string `json:"dnsName,omitempty"`
	// cidrSelector is the CIDR range to allow/deny traffic to. If this is set, dnsName must be unset.
	CIDRSelector string `json:"cidrSelector,omitempty"`
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
