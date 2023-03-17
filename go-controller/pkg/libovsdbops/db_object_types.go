package libovsdbops

const (
	addressSet dbObjType = iota
	// ACL here
)

const (
	// owner types
	EgressFirewallDNSOwnerType ownerType = "EgressFirewallDNS"
	EgressQoSOwnerType         ownerType = "EgressQoS"
	// only used for cleanup now, as the stale owner of network policy address sets
	NetworkPolicyOwnerType   ownerType = "NetworkPolicy"
	PodSelectorOwnerType     ownerType = "PodSelector"
	NamespaceOwnerType       ownerType = "Namespace"
	HybridNodeRouteOwnerType ownerType = "HybridNodeRoute"
	EgressIPOwnerType        ownerType = "EgressIP"
	EgressServiceOwnerType   ownerType = "EgressService"

	// owner extra IDs, make sure to define only 1 ExternalIDKey for every string value
	PriorityKey           ExternalIDKey = "priority"
	PolicyDirectionKey    ExternalIDKey = "direction"
	GressIdxKey           ExternalIDKey = "gress-index"
	AddressSetIPFamilyKey ExternalIDKey = "ip-family"
)

// ObjectIDsTypes should only be created here

var AddressSetEgressFirewallDNS = newObjectIDsType(addressSet, EgressFirewallDNSOwnerType, []ExternalIDKey{
	// dnsName
	ObjectNameKey,
	AddressSetIPFamilyKey,
})

var AddressSetHybridNodeRoute = newObjectIDsType(addressSet, HybridNodeRouteOwnerType, []ExternalIDKey{
	// nodeName
	ObjectNameKey,
	AddressSetIPFamilyKey,
})

var AddressSetEgressQoS = newObjectIDsType(addressSet, EgressQoSOwnerType, []ExternalIDKey{
	// namespace
	ObjectNameKey,
	// egress qos priority
	PriorityKey,
	AddressSetIPFamilyKey,
})

var AddressSetPodSelector = newObjectIDsType(addressSet, PodSelectorOwnerType, []ExternalIDKey{
	// pod selector string representation
	ObjectNameKey,
	AddressSetIPFamilyKey,
})

// deprecated, should only be used for sync
var AddressSetNetworkPolicy = newObjectIDsType(addressSet, NetworkPolicyOwnerType, []ExternalIDKey{
	// namespace_name
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
	// gress rule index
	GressIdxKey,
	AddressSetIPFamilyKey,
})

var AddressSetNamespace = newObjectIDsType(addressSet, NamespaceOwnerType, []ExternalIDKey{
	// namespace
	ObjectNameKey,
	AddressSetIPFamilyKey,
})

var AddressSetEgressIP = newObjectIDsType(addressSet, EgressIPOwnerType, []ExternalIDKey{
	// cluster-wide address set name
	ObjectNameKey,
	AddressSetIPFamilyKey,
})

var AddressSetEgressService = newObjectIDsType(addressSet, EgressServiceOwnerType, []ExternalIDKey{
	// cluster-wide address set name
	ObjectNameKey,
	AddressSetIPFamilyKey,
})
