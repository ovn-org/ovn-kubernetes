package types

import "time"

const (
	// Default network name
	DefaultNetworkName  = "default"
	K8sPrefix           = "k8s-"
	HybridOverlayPrefix = "int-"

	// K8sMgmtIntfName name to be used as an OVS internal port on the node
	K8sMgmtIntfName = "ovn-k8s-mp0"

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

	// ACL Priorities

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

	// ACL Tiers
	// Tier 0 is currently un-used and is a placeholder tier for future use cases (can be renamed when we have a use for it).
	// NOTE: When we upgrade from an OVN version without tiers to the new version with
	// tiers, all values in the new ACL.Tier column will be set to 0 a.k.a placeholder tier
	PlaceHolderACLTier = 0
	// Default Tier for all ACLs
	DefaultACLTier = 2

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
	EgressLiveMigrationReroutePiority     = 10

	V6NodeLocalNATSubnet           = "fd99::/64"
	V6NodeLocalNATSubnetPrefix     = 64
	V6NodeLocalNATSubnetNextHop    = "fd99::1"
	V6NodeLocalDistributedGWPortIP = "fd99::2"

	V4NodeLocalNATSubnet           = "169.254.0.0/20"
	V4NodeLocalNATSubnetPrefix     = 20
	V4NodeLocalNATSubnetNextHop    = "169.254.0.1"
	V4NodeLocalDistributedGWPortIP = "169.254.0.2"

	V4MasqueradeSubnet              = "169.254.169.0/29"
	V6MasqueradeSubnet              = "fd69::/125"
	V4HostMasqueradeIP              = "169.254.169.2"
	V6HostMasqueradeIP              = "fd69::2"
	V4OVNMasqueradeIP               = "169.254.169.1"
	V6OVNMasqueradeIP               = "fd69::1"
	V4HostETPLocalMasqueradeIP      = "169.254.169.3"
	V6HostETPLocalMasqueradeIP      = "fd69::3"
	V4DummyNextHopMasqueradeIP      = "169.254.169.4"
	V6DummyNextHopMasqueradeIP      = "fd69::4"
	V4OVNServiceHairpinMasqueradeIP = "169.254.169.5"
	V6OVNServiceHairpinMasqueradeIP = "fd69::5"

	// OpenFlow and Networking constants
	RouteAdvertisementICMPType    = 134
	NeighborAdvertisementICMPType = 136

	// Meter constants
	OvnACLLoggingMeter   = "acl-logging"
	OvnRateLimitingMeter = "rate-limiter"
	PacketsPerSecond     = "pktps"
	MeterAction          = "drop"

	// Default Meters created on GRs.
	OVNARPRateLimiter              = "arp"
	OVNARPResolveRateLimiter       = "arp-resolve"
	OVNBFDRateLimiter              = "bfd"
	OVNControllerEventsRateLimiter = "event-elb"
	OVNICMPV4ErrorsRateLimiter     = "icmp4-error"
	OVNICMPV6ErrorsRateLimiter     = "icmp6-error"
	OVNRejectRateLimiter           = "reject"
	OVNTCPRSTRateLimiter           = "tcp-reset"

	// OVN-K8S Topology Versions
	OvnSingleJoinSwitchTopoVersion = 1
	OvnNamespacedDenyPGTopoVersion = 2
	OvnHostToSvcOFTopoVersion      = 3
	OvnPortBindingTopoVersion      = 4
	OvnRoutingViaHostTopoVersion   = 5
	OvnCurrentTopologyVersion      = OvnRoutingViaHostTopoVersion

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
	// key for NAD name external-id, only used for secondary logical switch port of a pod
	NADExternalID = OvnK8sPrefix + "/" + "nad"
	// key for topology type external-id, only used for secondary network logical entities
	TopologyExternalID = OvnK8sPrefix + "/" + "topology"
	// key for topology version external-id
	TopologyVersionExternalID = "k8s-ovn-topo-version"
	// key for load_balancer kind external-id
	LoadBalancerKindExternalID = OvnK8sPrefix + "/" + "kind"
	// key for load_balancer service external-id
	LoadBalancerOwnerExternalID = OvnK8sPrefix + "/" + "owner"

	// different secondary network topology type defined in CNI netconf
	Layer3Topology   = "layer3"
	Layer2Topology   = "layer2"
	LocalnetTopology = "localnet"

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

	// InformerSyncTimeout is used to wait from the initial informer cache sync.
	// It allows ~4 list() retries with the default reflector exponential backoff config
	InformerSyncTimeout = 20 * time.Second
)
