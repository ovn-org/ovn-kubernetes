package types

import "time"

const (
	// Default network name
	DefaultNetworkName    = "default"
	K8sPrefix             = "k8s-"
	HybridOverlayPrefix   = "int-"
	HybridOverlayGRSubfix = "-gr"

	// K8sMgmtIntfNamePrefix name to be used as an OVS internal port on the node as prefix for networs
	K8sMgmtIntfNamePrefix = "ovn-k8s-mp"

	// UDNVRFDeviceSuffix vrf device suffix associated with every user defined primary network.
	UDNVRFDeviceSuffix = "-udn-vrf"
	// UDNVRFDevicePrefix vrf device prefix associated with every user
	UDNVRFDevicePrefix = "mp"

	// K8sMgmtIntfName name to be used as an OVS internal port on the node
	K8sMgmtIntfName = K8sMgmtIntfNamePrefix + "0"

	// PhysicalNetworkName is the name that maps to an OVS bridge that provides
	// access to physical/external network
	PhysicalNetworkName     = "physnet"
	PhysicalNetworkExGwName = "exgwphysnet"

	// LocalNetworkName is the name that maps to an OVS bridge that provides
	// access to local service
	LocalNetworkName = "locnet"

	// Local Bridge used for DGP access
	LocalBridgeName            = "br-local"
	LocalnetGatewayNextHopPort = "ovn-k8s-gw0"

	// OVS Bridge Datapath types
	DatapathUserspace = "netdev"

	// types.OVNClusterRouter is the name of the distributed router
	OVNClusterRouter = "ovn_cluster_router"
	OVNJoinSwitch    = "join"

	JoinSwitchPrefix             = "join_"
	ExternalSwitchPrefix         = "ext_"
	GWRouterPrefix               = "GR_"
	GWRouterLocalLBPostfix       = "_local"
	RouterToSwitchPrefix         = "rtos-"
	InterPrefix                  = "inter-"
	HybridSubnetPrefix           = "hybrid-subnet-"
	SwitchToRouterPrefix         = "stor-"
	JoinSwitchToGWRouterPrefix   = "jtor-"
	GWRouterToJoinSwitchPrefix   = "rtoj-"
	DistRouterToJoinSwitchPrefix = "dtoj-"
	JoinSwitchToDistRouterPrefix = "jtod-"
	EXTSwitchToGWRouterPrefix    = "etor-"
	GWRouterToExtSwitchPrefix    = "rtoe-"
	EgressGWSwitchPrefix         = "exgw-"
	PatchPortPrefix              = "patch-"
	PatchPortSuffix              = "-to-br-int"

	NodeLocalSwitch = "node_local_switch"

	// types.OVNLayer2Switch is the name of layer2 topology switch
	OVNLayer2Switch = "ovn_layer2_switch"
	// types.OVNLocalnetSwitch is the name of localnet topology switch
	OVNLocalnetSwitch = "ovn_localnet_switch"
	// types.OVNLocalnetPort is the name of localnet topology localnet port
	OVNLocalnetPort = "ovn_localnet_port"

	TransitSwitch               = "transit_switch"
	TransitSwitchToRouterPrefix = "tstor-"
	RouterToTransitSwitchPrefix = "rtots-"

	// ACL Default Tier Priorities

	// Default routed multicast allow acl rule priority
	DefaultRoutedMcastAllowPriority = 1013
	// Default multicast allow acl rule priority
	DefaultMcastAllowPriority = 1012
	// Default multicast deny acl rule priority
	DefaultMcastDenyPriority = 1011
	// Default allow acl rule priority
	DefaultAllowPriority = 1001
	// Default deny acl rule priority
	DefaultDenyPriority = 1000

	// ACL PlaceHolderACL Tier Priorities
	PrimaryUDNAllowPriority = 1001
	// Default deny acl rule priority
	PrimaryUDNDenyPriority = 1000

	// ACL Tiers
	// Tier 0 is called Primary as it is evaluated before any other feature-related Tiers.
	// Currently used for User Defined Network Feature.
	// NOTE: When we upgrade from an OVN version without tiers to the new version with
	// tiers, all values in the new ACL.Tier column will be set to 0.
	PrimaryACLTier = 0
	// Default Tier for all ACLs
	DefaultACLTier = 2
	// Default Tier for all ACLs belonging to Admin Network Policy
	DefaultANPACLTier = 1
	// Default Tier for all ACLs belonging to Baseline Admin Network Policy
	DefaultBANPACLTier = 3

	// priority of logical router policies on the OVNClusterRouter
	EgressFirewallStartPriority           = 10000
	MinimumReservedEgressFirewallPriority = 2000
	MGMTPortPolicyPriority                = "1005"
	NodeSubnetPolicyPriority              = "1004"
	InterNodePolicyPriority               = "1003"
	HybridOverlaySubnetPriority           = 1002
	HybridOverlayReroutePriority          = 501
	DefaultNoRereoutePriority             = 102
	EgressSVCReroutePriority              = 101
	EgressIPReroutePriority               = 100
	EgressIPRerouteQoSRulePriority        = 103
	EgressLiveMigrationReroutePiority     = 10

	// EndpointSliceMirrorControllerName mirror EndpointSlice controller name (used as a value for the "endpointslice.kubernetes.io/managed-by" label)
	EndpointSliceMirrorControllerName = "endpointslice-mirror-controller.k8s.ovn.org"
	// EndpointSliceDefaultControllerName default kubernetes EndpointSlice controller name (used as a value for the "endpointslice.kubernetes.io/managed-by" label)
	EndpointSliceDefaultControllerName = "endpointslice-controller.k8s.io"
	// LabelSourceEndpointSlice label key used in mirrored EndpointSlice
	// that has the value of the default EndpointSlice name
	LabelSourceEndpointSlice = "k8s.ovn.org/source-endpointslice"
	// LabelSourceEndpointSliceVersion label key used in mirrored EndpointSlice
	// that has the value of the last known default EndpointSlice ResourceVersion
	LabelSourceEndpointSliceVersion = "k8s.ovn.org/source-endpointslice-version"
	// LabelUserDefinedEndpointSliceNetwork label key used in mirrored EndpointSlices that contains the current primary user defined network name
	LabelUserDefinedEndpointSliceNetwork = "k8s.ovn.org/endpointslice-network"
	// LabelUserDefinedServiceName label key used in mirrored EndpointSlices that contains the service name matching the EndpointSlice
	LabelUserDefinedServiceName = "k8s.ovn.org/service-name"

	// Packet marking
	EgressIPNodeConnectionMark         = "1008"
	EgressIPReplyTrafficConnectionMark = 42

	// primary user defined network's default join subnet value
	// users can configure custom values using NADs
	UserDefinedPrimaryNetworkJoinSubnetV4 = "100.65.0.0/16"
	UserDefinedPrimaryNetworkJoinSubnetV6 = "fd99::/64"

	// OpenFlow and Networking constants
	RouteAdvertisementICMPType    = 134
	NeighborAdvertisementICMPType = 136

	// Meter constants
	OvnACLLoggingMeter   = "acl-logging"
	OvnRateLimitingMeter = "rate-limiter"
	PacketsPerSecond     = "pktps"
	MeterAction          = "drop"

	// OVN-K8S annotation & taint constants
	OvnK8sPrefix = "k8s.ovn.org"
	// Deprecated: we used to set topology version as an annotation on the node. We don't do this anymore.
	OvnK8sTopoAnno         = OvnK8sPrefix + "/" + "topology-version"
	OvnK8sSmallMTUTaintKey = OvnK8sPrefix + "/" + "mtu-too-small"

	// name of the configmap used to synchronize status (e.g. watch for topology changes)
	OvnK8sStatusCMName         = "control-plane-status"
	OvnK8sStatusKeyTopoVersion = "topology-version"

	// Monitoring constants
	SFlowAgent = "ovn-k8s-mp0"

	// OVNKube-Node Node types
	NodeModeFull    = "full"
	NodeModeDPU     = "dpu"
	NodeModeDPUHost = "dpu-host"

	// Geneve header length for IPv4 (https://github.com/openshift/cluster-network-operator/pull/720#issuecomment-664020823)
	GeneveHeaderLengthIPv4 = 58
	// Geneve header length for IPv6 (https://github.com/openshift/cluster-network-operator/pull/720#issuecomment-664020823)
	GeneveHeaderLengthIPv6 = GeneveHeaderLengthIPv4 + 20

	ClusterPortGroupNameBase    = "clusterPortGroup"
	ClusterRtrPortGroupNameBase = "clusterRtrPortGroup"

	OVSDBTimeout     = 10 * time.Second
	OVSDBWaitTimeout = 0

	ClusterLBGroupName       = "clusterLBGroup"
	ClusterSwitchLBGroupName = "clusterSwitchLBGroup"
	ClusterRouterLBGroupName = "clusterRouterLBGroup"

	// key for network name external-id
	NetworkExternalID = OvnK8sPrefix + "/" + "network"
	// key for network role external-id: possible values are "default", "primary", "secondary"
	NetworkRoleExternalID = OvnK8sPrefix + "/" + "role"
	// key for NAD name external-id, only used for secondary logical switch port of a pod
	// key for network name external-id
	NADExternalID = OvnK8sPrefix + "/" + "nad"
	// key for topology type external-id, only used for secondary network logical entities
	TopologyExternalID = OvnK8sPrefix + "/" + "topology"
	// key for load_balancer kind external-id
	LoadBalancerKindExternalID = OvnK8sPrefix + "/" + "kind"
	// key for load_balancer service external-id
	LoadBalancerOwnerExternalID = OvnK8sPrefix + "/" + "owner"

	// different secondary network topology type defined in CNI netconf
	Layer3Topology   = "layer3"
	Layer2Topology   = "layer2"
	LocalnetTopology = "localnet"

	// different types of network roles
	// defined in CNI netconf as a user defined network
	NetworkRolePrimary   = "primary"
	NetworkRoleSecondary = "secondary"
	NetworkRoleDefault   = "default"
	// defined internally by ovnkube to recognize "default"
	// network's role as a "infrastructure-locked" network
	// when user defined network is the primary network for
	// the pod which makes "default" network niether primary
	// nor secondary
	NetworkRoleInfrastructure = "infrastructure-locked"

	// db index keys
	// PrimaryIDKey is used as a primary client index
	PrimaryIDKey = OvnK8sPrefix + "/id"

	OvnDefaultZone = "global"

	// EgressService "reserved" hosts - when set on an EgressService they have a special meaning

	EgressServiceNoHost     = ""    // set on services with no allocated node
	EgressServiceNoSNATHost = "ALL" // set on services with sourceIPBy=Network

	// MaxLogicalPortTunnelKey is maximum tunnel key that can be requested for a
	// Logical Switch or Router Port
	MaxLogicalPortTunnelKey = 32767

	// InformerSyncTimeout is used when waiting for the initial informer cache sync
	// (i.e. all existing objects should be listed by the informer).
	// It allows ~4 list() retries with the default reflector exponential backoff config
	InformerSyncTimeout = 20 * time.Second

	// HandlerSyncTimeout is used when waiting for initial object handler sync.
	// (i.e. all the ADD events should be processed for the existing objects by the event handler)
	HandlerSyncTimeout = 20 * time.Second

	// GRMACBindingAgeThreshold is the lifetime in seconds of each MAC binding
	// entry for the gateway routers. After this time, the entry is removed and
	// may be refreshed with a new ARP request.
	GRMACBindingAgeThreshold = "300"
)
