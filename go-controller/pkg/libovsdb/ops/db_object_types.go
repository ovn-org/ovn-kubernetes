package ops

import "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

const (
	addressSet dbObjType = iota
	acl
	dhcpOptions
	portGroup
	logicalRouterPolicy
	qos
	nat
)

const (
	// owner types
	EgressFirewallDNSOwnerType          ownerType = "EgressFirewallDNS"
	EgressFirewallOwnerType             ownerType = "EgressFirewall"
	EgressQoSOwnerType                  ownerType = "EgressQoS"
	AdminNetworkPolicyOwnerType         ownerType = "AdminNetworkPolicy"
	BaselineAdminNetworkPolicyOwnerType ownerType = "BaselineAdminNetworkPolicy"
	// NetworkPolicyOwnerType is deprecated for address sets, should only be used for sync.
	// New owner of network policy address sets, is PodSelectorOwnerType.
	NetworkPolicyOwnerType ownerType = "NetworkPolicy"
	NetpolDefaultOwnerType ownerType = "NetpolDefault"
	PodSelectorOwnerType   ownerType = "PodSelector"
	NamespaceOwnerType     ownerType = "Namespace"
	// HybridNodeRouteOwnerType is transferred from egressgw to apbRoute controller with the same dbIDs
	HybridNodeRouteOwnerType    ownerType = "HybridNodeRoute"
	EgressIPOwnerType           ownerType = "EgressIP"
	EgressServiceOwnerType      ownerType = "EgressService"
	MulticastNamespaceOwnerType ownerType = "MulticastNS"
	MulticastClusterOwnerType   ownerType = "MulticastCluster"
	NetpolNodeOwnerType         ownerType = "NetpolNode"
	NetpolNamespaceOwnerType    ownerType = "NetpolNamespace"
	VirtualMachineOwnerType     ownerType = "VirtualMachine"
	UDNEnabledServiceOwnerType  ownerType = "UDNEnabledService"
	// NetworkPolicyPortIndexOwnerType is the old version of NetworkPolicyOwnerType, kept for sync only
	NetworkPolicyPortIndexOwnerType ownerType = "NetworkPolicyPortIndexOwnerType"
	// ClusterOwnerType means the object is cluster-scoped and doesn't belong to any k8s objects
	ClusterOwnerType ownerType = "Cluster"
	// UDNIsolationOwnerType means the object is needed to implement UserDefinedNetwork isolation
	UDNIsolationOwnerType ownerType = "UDNIsolation"

	// owner extra IDs, make sure to define only 1 ExternalIDKey for every string value
	PriorityKey           ExternalIDKey = "priority"
	PolicyDirectionKey    ExternalIDKey = "direction"
	GressIdxKey           ExternalIDKey = "gress-index"
	IPFamilyKey           ExternalIDKey = "ip-family"
	NetworkKey            ExternalIDKey = "network"
	TypeKey               ExternalIDKey = "type"
	IpKey                 ExternalIDKey = "ip"
	PortPolicyIndexKey    ExternalIDKey = "port-policy-index"
	IpBlockIndexKey       ExternalIDKey = "ip-block-index"
	RuleIndex             ExternalIDKey = "rule-index"
	CIDRKey               ExternalIDKey = types.OvnK8sPrefix + "/cidr"
	PortPolicyProtocolKey ExternalIDKey = "port-policy-protocol"
)

// ObjectIDsTypes should only be created here

var AddressSetAdminNetworkPolicy = newObjectIDsType(addressSet, AdminNetworkPolicyOwnerType, []ExternalIDKey{
	// anp name
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
	// gress rule's index
	GressIdxKey,
	IPFamilyKey,
})

var AddressSetBaselineAdminNetworkPolicy = newObjectIDsType(addressSet, BaselineAdminNetworkPolicyOwnerType, []ExternalIDKey{
	// banp name
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
	// gress rule's index
	GressIdxKey,
	IPFamilyKey,
})

var AddressSetEgressFirewallDNS = newObjectIDsType(addressSet, EgressFirewallDNSOwnerType, []ExternalIDKey{
	// dnsName
	ObjectNameKey,
	IPFamilyKey,
})

var AddressSetHybridNodeRoute = newObjectIDsType(addressSet, HybridNodeRouteOwnerType, []ExternalIDKey{
	// nodeName
	ObjectNameKey,
	IPFamilyKey,
})

var AddressSetEgressQoS = newObjectIDsType(addressSet, EgressQoSOwnerType, []ExternalIDKey{
	// namespace
	ObjectNameKey,
	// egress qos priority
	PriorityKey,
	IPFamilyKey,
})

var AddressSetPodSelector = newObjectIDsType(addressSet, PodSelectorOwnerType, []ExternalIDKey{
	// pod selector string representation
	ObjectNameKey,
	IPFamilyKey,
})

// deprecated, should only be used for sync
var AddressSetNetworkPolicy = newObjectIDsType(addressSet, NetworkPolicyOwnerType, []ExternalIDKey{
	// namespace_name
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
	// gress rule index
	GressIdxKey,
	IPFamilyKey,
})

var AddressSetNamespace = newObjectIDsType(addressSet, NamespaceOwnerType, []ExternalIDKey{
	// namespace
	ObjectNameKey,
	IPFamilyKey,
})

var AddressSetEgressIP = newObjectIDsType(addressSet, EgressIPOwnerType, []ExternalIDKey{
	// cluster-wide address set name
	ObjectNameKey,
	IPFamilyKey,
	NetworkKey,
})

var AddressSetEgressService = newObjectIDsType(addressSet, EgressServiceOwnerType, []ExternalIDKey{
	// cluster-wide address set name
	ObjectNameKey,
	IPFamilyKey,
})

var AddressSetUDNEnabledService = newObjectIDsType(addressSet, UDNEnabledServiceOwnerType, []ExternalIDKey{
	// cluster-wide address set name
	ObjectNameKey,
	IPFamilyKey,
})

var ACLAdminNetworkPolicy = newObjectIDsType(acl, AdminNetworkPolicyOwnerType, []ExternalIDKey{
	// anp name
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
	// gress rule's index
	GressIdxKey,
	// gress rule's peer port's protocol index
	PortPolicyProtocolKey,
})

var ACLBaselineAdminNetworkPolicy = newObjectIDsType(acl, BaselineAdminNetworkPolicyOwnerType, []ExternalIDKey{
	// banp name
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
	// gress rule's index
	GressIdxKey,
	// gress rule's peer port's protocol index
	PortPolicyProtocolKey,
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

// ACLNetworkPolicyPortIndex define a unique index for every network policy ACL.
// ingress/egress + NetworkPolicy[In/E]gressRule idx - defines given gressPolicy.
// ACLs are created for every gp.portPolicies:
// - for empty policy (no selectors and no ip blocks) - empty ACL (see allIPsMatch)
// OR
// - all selector-based peers ACL
// - for every IPBlock +1 ACL
// Therefore unique id for a given gressPolicy is portPolicy idx + IPBlock idx
// (empty policy and all selector-based peers ACLs will have idx=-1)
// Note: keep for backward compatibility only
// Deprecated, should only be used for sync
var ACLNetworkPolicyPortIndex = newObjectIDsType(acl, NetworkPolicyPortIndexOwnerType, []ExternalIDKey{
	// policy namespace+name
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
	// gress rule index
	GressIdxKey,
	PortPolicyIndexKey,
	IpBlockIndexKey,
})

// ACLNetworkPolicy define a unique index for every network policy ACL.
// ingress/egress + NetworkPolicy[In/E]gressRule idx - defines given gressPolicy.
// ACLs are created for gp.portPolicies which are grouped by protocol:
// - for empty policy (no selectors and no ip blocks) - empty ACL (see allIPsMatch)
// OR
// - all selector-based peers ACL
// - for every IPBlock +1 ACL
// Therefore unique id for a given gressPolicy is protocol name + IPBlock idx
// (protocol will be "None" if no port policy is defined, and empty policy and all
// selector-based peers ACLs will have idx=-1)
var ACLNetworkPolicy = newObjectIDsType(acl, NetworkPolicyOwnerType, []ExternalIDKey{
	// policy namespace+name
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
	// gress rule index
	GressIdxKey,
	PortPolicyProtocolKey,
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

var ACLUDN = newObjectIDsType(acl, UDNIsolationOwnerType, []ExternalIDKey{
	// name of a UDN-related ACL
	ObjectNameKey,
	// egress or ingress
	PolicyDirectionKey,
})

var VirtualMachineDHCPOptions = newObjectIDsType(dhcpOptions, VirtualMachineOwnerType, []ExternalIDKey{
	// We can have multiple VMs with same CIDR they  may have different
	// hostname.
	// vm "namespace/name"
	ObjectNameKey,
	// CIDR field from DHCPOptions with ":" replaced by "."
	CIDRKey,
})

var PortGroupNamespace = newObjectIDsType(portGroup, NamespaceOwnerType, []ExternalIDKey{
	// namespace name
	ObjectNameKey,
})

// every namespace that has at least 1 network policy, has resources that are shared by all network policies
// in that namespace.
var PortGroupNetpolNamespace = newObjectIDsType(portGroup, NetpolNamespaceOwnerType, []ExternalIDKey{
	// namespace
	ObjectNameKey,
	// in the same namespace there can be 2 default deny port groups, egress and ingress
	PolicyDirectionKey,
})

var PortGroupNetworkPolicy = newObjectIDsType(portGroup, NetworkPolicyOwnerType, []ExternalIDKey{
	// policy namespace+name
	ObjectNameKey,
})

var PortGroupAdminNetworkPolicy = newObjectIDsType(portGroup, AdminNetworkPolicyOwnerType, []ExternalIDKey{
	// ANP name
	ObjectNameKey,
})

var PortGroupBaselineAdminNetworkPolicy = newObjectIDsType(portGroup, BaselineAdminNetworkPolicyOwnerType, []ExternalIDKey{
	// BANP name
	ObjectNameKey,
})

var PortGroupCluster = newObjectIDsType(portGroup, ClusterOwnerType, []ExternalIDKey{
	// name of a global port group
	// currently ClusterPortGroup and ClusterRtrPortGroup are present
	ObjectNameKey,
})

var PortGroupUDN = newObjectIDsType(portGroup, UDNIsolationOwnerType, []ExternalIDKey{
	// name of a UDN port group
	// currently uses:
	// secondaryPods - on default network switch to distinguish non-primary pods
	ObjectNameKey,
})

var LogicalRouterPolicyEgressIP = newObjectIDsType(logicalRouterPolicy, EgressIPOwnerType, []ExternalIDKey{
	// the priority of the LRP
	PriorityKey,
	// for the reroute policies it should be the "EIPName_Namespace/podName"
	// for the no-reroute global policies it should be the unique global name
	ObjectNameKey,
	// the IP Family for this policy, ip4 or ip6 or ip(dualstack)
	IPFamilyKey,
	NetworkKey,
})

var NATEgressIP = newObjectIDsType(nat, EgressIPOwnerType, []ExternalIDKey{
	// for the NAT policy, it should be the "EIPName_Namespace/podName"
	ObjectNameKey,
	// the IP Family for this policy, ip4 or ip6 or ip(dualstack)
	IPFamilyKey,
})

var QoSEgressQoS = newObjectIDsType(qos, EgressQoSOwnerType, []ExternalIDKey{
	// the priority of the QoSRule (OVN priority is the same as the rule index priority for this feature)
	// this value will be unique in a given namespace
	PriorityKey,
	// namespace
	ObjectNameKey,
})

var QoSRuleEgressIP = newObjectIDsType(qos, EgressIPOwnerType, []ExternalIDKey{
	// the priority of the QoSRule
	PriorityKey,
	// should be the unique global name
	ObjectNameKey,
	// the IP Family for this policy, ip4 or ip6 or ip(dualstack)
	IPFamilyKey,
})
