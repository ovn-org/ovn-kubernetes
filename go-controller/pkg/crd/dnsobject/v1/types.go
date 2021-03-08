package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +resource:path=dnsobject
// +kubebuilder:resource:scope=Cluster
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:printcolumn:name="dnsObject Status",type=string,JSONPath=".status.status"
// EgressFirewallDNS describes the current egressFirewallDNS entries for the cluster.
// This is a centralized object to coordinate egressFirewallDNS entries. all nodes
// update this object and the master node accesses the ovn database when needed.
type DNSObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Specification of the desired behavior of EgressFirewallDNS
	Spec DNSObjectSpec `json:"spec,omitempty"`

	// Observed status of EgressFirewallDNS
	// +optional
	Status DNSObjectStatus `json:"status,omitempty"`
}

type DNSObjectSpec struct {
	DNSObjectEntries map[string]DNSObjectEntry `json:"egressdnsEntries,omitempty"`
}

type DNSObjectEntry struct {
	IPAddresses []string `json:"ipAddresses,omitempty"`
}

type DNSObjectStatus struct {
	Status string `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +resource:path=dnsobject
// DNSObjectList is the list of dnsObjects
type DNSObjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []DNSObject `json:"items"`
}
