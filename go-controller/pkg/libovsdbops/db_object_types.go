package libovsdbops

import "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

const (
	addressSet dbObjType = iota
	acl
	dhcpOptions
)

const (
	// owner types
	EgressFirewallDNSOwnerType ownerType = "EgressFirewallDNS"
	EgressFirewallOwnerType    ownerType = "EgressFirewall"
	EgressQoSOwnerType         ownerType = "EgressQoS"
	// NetworkPolicyOwnerType is deprecated for address sets, should only be used for sync.
	// New owner of network policy address sets, is PodSelectorOwnerType.
	NetworkPolicyOwnerType      ownerType = "NetworkPolicy"
	NetpolDefaultOwnerType      ownerType = "NetpolDefault"
	PodSelectorOwnerType        ownerType = "PodSelector"
	NamespaceOwnerType          ownerType = "Namespace"
	HybridNodeRouteOwnerType    ownerType = "HybridNodeRoute"
	EgressIPOwnerType           ownerType = "EgressIP"
	EgressServiceOwnerType      ownerType = "EgressService"
	MulticastNamespaceOwnerType ownerType = "MulticastNS"
	MulticastClusterOwnerType   ownerType = "MulticastCluster"
	NetpolNodeOwnerType         ownerType = "NetpolNode"
	NetpolNamespaceOwnerType    ownerType = "NetpolNamespace"
	VirtualMachineOwnerType     ownerType = "VirtualMachine"

	// owner extra IDs, make sure to define only 1 ExternalIDKey for every string value
	PriorityKey           ExternalIDKey = "priority"
	PolicyDirectionKey    ExternalIDKey = "direction"
	GressIdxKey           ExternalIDKey = "gress-index"
	AddressSetIPFamilyKey ExternalIDKey = "ip-family"
	TypeKey               ExternalIDKey = "type"
	IpKey                 ExternalIDKey = "ip"
	PortPolicyIndexKey    ExternalIDKey = "port-policy-index"
	IpBlockIndexKey       ExternalIDKey = "ip-block-index"
	RuleIndex             ExternalIDKey = "rule-index"
	VirtualMachineKey     ExternalIDKey = types.OvnK8sPrefix + "/vm"
	NamespaceKey          ExternalIDKey = types.OvnK8sPrefix + "/namespace"
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

var ACLNetpolDefault = newObjectIDsType(acl, NetpolDefaultOwnerType, []ExternalIDKey{
	// for now there is only 1 acl of this type, but we use a name in case more types are needed in the future
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
})

var ACLMulticastNamespace = newObjectIDsType(acl, MulticastNamespaceOwnerType, []ExternalIDKey{
	// namespace
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
})

var ACLMulticastCluster = newObjectIDsType(acl, MulticastClusterOwnerType, []ExternalIDKey{
	// cluster-scoped multicast acls
	// there are 2 possible TypeKey values for cluster default multicast acl: DefaultDeny and AllowInterNode
	TypeKey,
	// egress or ingress
	PolicyDirectionKey,
})

var ACLNetpolNode = newObjectIDsType(acl, NetpolNodeOwnerType, []ExternalIDKey{
	// node name
	ObjectNameKey,
	// exact ip for management port, every node may have more than 1 management ip
	IpKey,
})

// ACLNetworkPolicy define a unique index for every network policy ACL.
// ingress/egress + NetworkPolicy[In/E]gressRule idx - defines given gressPolicy.
// ACLs are created for every gp.portPolicies:
// - for empty policy (no selectors and no ip blocks) - empty ACL (see allIPsMatch)
// OR
// - all selector-based peers ACL
// - for every IPBlock +1 ACL
// Therefore unique id for a given gressPolicy is portPolicy idx + IPBlock idx
// (empty policy and all selector-based peers ACLs will have idx=-1)
var ACLNetworkPolicy = newObjectIDsType(acl, NetworkPolicyOwnerType, []ExternalIDKey{
	// policy namespace+name
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
	// gress rule index
	GressIdxKey,
	PortPolicyIndexKey,
	IpBlockIndexKey,
})

var ACLNetpolNamespace = newObjectIDsType(acl, NetpolNamespaceOwnerType, []ExternalIDKey{
	// namespace
	ObjectNameKey,
	// in the same namespace there can be 2 default deny port groups, egress and ingress
	PolicyDirectionKey,
	// every port group has default deny and arp allow acl.
	TypeKey,
})

var ACLEgressFirewall = newObjectIDsType(acl, EgressFirewallOwnerType, []ExternalIDKey{
	// namespace
	ObjectNameKey,
	// there can only be 1 egress firewall object in every namespace, named "default"
	// The only additional id we need is the index of the EgressFirewall.Spec.Egress rule.
	RuleIndex,
})

var VirtualMachineDHCPOptions = newObjectIDsType(dhcpOptions, VirtualMachineOwnerType, []ExternalIDKey{
	// cidr
	ObjectNameKey,
	// We can have multiple VMs with same CIDR they  may have different
	// hostname
	VirtualMachineKey,
	// Also differente namespace so we have on DHCPOption per LSP
	NamespaceKey,
})
