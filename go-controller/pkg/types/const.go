package types

import "time"

const (
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

	V6NodeLocalNATSubnet           = "fd99::/64"
	V6NodeLocalNATSubnetPrefix     = 64
	V6NodeLocalNATSubnetNextHop    = "fd99::1"
	V6NodeLocalDistributedGWPortIP = "fd99::2"

	V4NodeLocalNATSubnet           = "169.254.0.0/20"
	V4NodeLocalNATSubnetPrefix     = 20
	V4NodeLocalNATSubnetNextHop    = "169.254.0.1"
	V4NodeLocalDistributedGWPortIP = "169.254.0.2"

	V4MasqueradeSubnet         = "169.254.169.0/30"
	V4HostMasqueradeIP         = "169.254.169.2"
	V6HostMasqueradeIP         = "fd69::2"
	V4OVNMasqueradeIP          = "169.254.169.1"
	V6OVNMasqueradeIP          = "fd69::1"
	V4HostETPLocalMasqueradeIP = "169.254.169.3"
	V6HostETPLocalMasqueradeIP = "fd69::3"

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

	// OVN-K8S Address Sets Names
	HybridRoutePolicyPrefix = "hybrid-route-pods-"
	EgressQoSRulePrefix     = "egress-qos-pods-"

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

	ClusterPortGroupName    = "clusterPortGroup"
	ClusterRtrPortGroupName = "clusterRtrPortGroup"

	OVSDBTimeout     = 10 * time.Second
	OVSDBWaitTimeout = 0

	ClusterLBGroupName = "clusterLBGroup"
)
