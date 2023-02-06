package base_network_controller

import (
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
)

// NamespaceInfo contains information related to a Namespace. Use oc.GetNamespaceLocked()
// or oc.waitForNamespaceLocked() to get a locked NamespaceInfo for a Namespace, and call
// nsInfo.Unlock() on it when you are done with it. (No code outside of the code that
// manages the oc.namespaces map is ever allowed to hold an unlocked NamespaceInfo.)
type NamespaceInfo struct {
	sync.RWMutex

	// addressSet is an address set object that holds the IP addresses
	// of all pods in the namespace.
	AddressSet addressset.AddressSet

	// Map of related network policies. Policy will add itself to this list when it's ready to subscribe
	// to namespace Update events. Retry logic to update network policy based on namespace event is handled by namespace.
	// Policy should only be added after successful create, and deleted before any network policy resources are deleted.
	// This is the map of keys that can be used to get networkPolicy from oc.networkPolicies.
	//
	// You must hold the NamespaceInfo's mutex to add/delete dependent policies.
	// Namespace can take oc.networkPolicies key Lock while holding nsInfo lock, the opposite should never happen.
	RelatedNetworkPolicies map[string]bool

	// RoutingExternalGWs is a slice of net.IP containing the values parsed from
	// annotation k8s.ovn.org/routing-external-gws
	RoutingExternalGWs GatewayInfo

	// RoutingExternalPodGWs contains a map of all pods serving as exgws as well as their
	// exgw IPs
	// key is <namespace>_<pod name>
	RoutingExternalPodGWs map[string]GatewayInfo

	MulticastEnabled bool

	// If not empty, then it has to be set to a logging a severity level, e.g. "notice", "alert", etc
	AclLogging ACLLoggingLevels
}

type GatewayInfo struct {
	GWs        sets.String
	BFDEnabled bool
}

// ACL logging severity levels
type ACLLoggingLevels struct {
	Allow string `json:"allow,omitempty"`
	Deny  string `json:"deny,omitempty"`
}

// NodeSyncs structure contains flags for the different failures
// so the retry logic can control what need to retry based
type NodeSyncs struct {
	SyncNode              bool
	SyncClusterRouterPort bool
	SyncMgmtPort          bool
	SyncGw                bool
	SyncHo                bool
}
