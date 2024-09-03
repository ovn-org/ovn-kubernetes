package config

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/urfave/cli/v2"
	gcfg "gopkg.in/gcfg.v1"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
	"k8s.io/klog/v2"

	kexec "k8s.io/utils/exec"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// DefaultEncapPort number used if not supplied
const DefaultEncapPort = 6081

const DefaultAPIServer = "http://localhost:8443"

// Default IANA-assigned UDP port number for VXLAN
const DefaultVXLANPort = 4789

// The following are global config parameters that other modules may access directly
var (
	// Build information. Populated at build-time.
	// commit ID used to build ovn-kubernetes
	Commit = ""
	// branch used to build ovn-kubernetes
	Branch = ""
	// ovn-kubernetes build user
	BuildUser = ""
	// ovn-kubernetes build date
	BuildDate = ""
	// ovn-kubernetes version, to be changed with every release
	Version = "1.0.0"
	// version of the go runtime used to compile ovn-kubernetes
	GoVersion = runtime.Version()
	// os and architecture used to build ovn-kubernetes
	OSArch = fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)

	// ovn-kubernetes cni config file name
	CNIConfFileName = "10-ovn-kubernetes.conf"

	// Default holds parsed config file parameters and command-line overrides
	Default = DefaultConfig{
		MTU:                   1400,
		ConntrackZone:         64000,
		EncapType:             "geneve",
		EncapIP:               "",
		EncapPort:             DefaultEncapPort,
		InactivityProbe:       100000, // in Milliseconds
		OpenFlowProbe:         180,    // in Seconds
		OfctrlWaitBeforeClear: 0,      // in Milliseconds
		MonitorAll:            true,
		LFlowCacheEnable:      true,
		RawClusterSubnets:     "10.128.0.0/14/23",
		Zone:                  types.OvnDefaultZone,
	}

	// Logging holds logging-related parsed config file parameters and command-line overrides
	Logging = LoggingConfig{
		File:                "", // do not log to a file by default
		CNIFile:             "",
		LibovsdbFile:        "",
		Level:               4,
		LogFileMaxSize:      100, // Size in Megabytes
		LogFileMaxBackups:   5,
		LogFileMaxAge:       5, //days
		ACLLoggingRateLimit: 20,
	}

	// Monitoring holds monitoring-related parsed config file parameters and command-line overrides
	Monitoring = MonitoringConfig{
		RawNetFlowTargets: "",
		RawSFlowTargets:   "",
		RawIPFIXTargets:   "",
	}

	// IPFIX holds IPFIX-related performance configuration options. It requires that the
	// IPFIXTargets value of the Monitoring section contains at least one endpoint.
	IPFIX = IPFIXConfig{
		Sampling:           400,
		CacheActiveTimeout: 60,
		CacheMaxFlows:      0,
	}

	// CNI holds CNI-related parsed config file parameters and command-line overrides
	CNI = CNIConfig{
		ConfDir: "/etc/cni/net.d",
		Plugin:  "ovn-k8s-cni-overlay",
	}

	// Kubernetes holds Kubernetes-related parsed config file parameters and command-line overrides
	Kubernetes = KubernetesConfig{
		APIServer:               DefaultAPIServer,
		RawServiceCIDRs:         "172.16.1.0/24",
		OVNConfigNamespace:      "ovn-kubernetes",
		HostNetworkNamespace:    "",
		DisableRequestedChassis: false,
		PlatformType:            "",
		DNSServiceNamespace:     "kube-system",
		DNSServiceName:          "kube-dns",
		// By default, use a short lifetime length for certificates to ensure that the automatic rotation works well,
		// might revisit in the future to use a more sensible value
		CertDuration: 10 * time.Minute,
	}

	// Metrics holds Prometheus metrics-related parameters.
	Metrics MetricsConfig

	// OVNKubernetesFeatureConfig holds OVN-Kubernetes feature enhancement config file parameters and command-line overrides
	OVNKubernetesFeature = OVNKubernetesFeatureConfig{
		EgressIPReachabiltyTotalTimeout: 1,
	}

	// OvnNorth holds northbound OVN database client and server authentication and location details
	OvnNorth OvnAuthConfig

	// OvnSouth holds southbound OVN database client and server authentication and location details
	OvnSouth OvnAuthConfig

	// Gateway holds node gateway-related parsed config file parameters and command-line overrides
	Gateway = GatewayConfig{
		V4JoinSubnet:       "100.64.0.0/16",
		V6JoinSubnet:       "fd98::/64",
		V4MasqueradeSubnet: "169.254.169.0/29",
		V6MasqueradeSubnet: "fd69::/125",
		MasqueradeIPs: MasqueradeIPsConfig{
			V4OVNMasqueradeIP:               net.ParseIP("169.254.169.1"),
			V6OVNMasqueradeIP:               net.ParseIP("fd69::1"),
			V4HostMasqueradeIP:              net.ParseIP("169.254.169.2"),
			V6HostMasqueradeIP:              net.ParseIP("fd69::2"),
			V4HostETPLocalMasqueradeIP:      net.ParseIP("169.254.169.3"),
			V6HostETPLocalMasqueradeIP:      net.ParseIP("fd69::3"),
			V4DummyNextHopMasqueradeIP:      net.ParseIP("169.254.169.4"),
			V6DummyNextHopMasqueradeIP:      net.ParseIP("fd69::4"),
			V4OVNServiceHairpinMasqueradeIP: net.ParseIP("169.254.169.5"),
			V6OVNServiceHairpinMasqueradeIP: net.ParseIP("fd69::5"),
		},
	}

	// Set Leaderelection config values based on
	// https://github.com/openshift/enhancements/blame/84e894ead7b188a1013556e0ba6973b8463995f1/CONVENTIONS.md#L183

	// MasterHA holds master HA related config options.
	MasterHA = HAConfig{
		ElectionRetryPeriod:   26,
		ElectionRenewDeadline: 107,
		ElectionLeaseDuration: 137,
	}

	// ClusterMgrHA holds cluster manager HA related config options.
	ClusterMgrHA = HAConfig{
		ElectionRetryPeriod:   26,
		ElectionRenewDeadline: 107,
		ElectionLeaseDuration: 137,
	}

	// HybridOverlay holds hybrid overlay feature config options.
	HybridOverlay = HybridOverlayConfig{
		VXLANPort: DefaultVXLANPort,
	}

	// UnprivilegedMode allows ovnkube-node to run without SYS_ADMIN capability, by performing interface setup in the CNI plugin
	UnprivilegedMode bool

	// EnableMulticast enables multicast support between the pods within the same namespace
	EnableMulticast bool

	// IPv4Mode captures whether we are using IPv4 for OVN logical topology. (ie, single-stack IPv4 or dual-stack)
	IPv4Mode bool

	// IPv6Mode captures whether we are using IPv6 for OVN logical topology. (ie, single-stack IPv6 or dual-stack)
	IPv6Mode bool

	// OvnKubeNode holds ovnkube-node parsed config file parameters and command-line overrides
	OvnKubeNode = OvnKubeNodeConfig{
		Mode: types.NodeModeFull,
	}

	ClusterManager = ClusterManagerConfig{
		V4TransitSwitchSubnet: "100.88.0.0/16",
		V6TransitSwitchSubnet: "fd97::/64",
	}
)

const (
	kubeServiceAccountPath       string = "/var/run/secrets/kubernetes.io/serviceaccount/"
	kubeServiceAccountFileToken  string = "token"
	kubeServiceAccountFileCACert string = "ca.crt"
)

// DefaultConfig holds parsed config file parameters and command-line overrides
type DefaultConfig struct {
	// MTU value used for the overlay networks.
	MTU int `gcfg:"mtu"`
	// RoutableMTU is the maximum routable MTU between nodes, used to facilitate
	// an MTU migration procedure where different nodes might be using different
	// MTU values
	RoutableMTU int `gcfg:"routable-mtu"`
	// ConntrackZone affects only the gateway nodes, This value is used to track connections
	// that are initiated from the pods so that the reverse connections go back to the pods.
	// This represents the conntrack zone used for the conntrack flow rules.
	ConntrackZone int `gcfg:"conntrack-zone"`
	// HostMasqConntrackZone is an unexposed config with the value of ConntrackZone+1
	HostMasqConntrackZone int
	// OVNMasqConntrackZone is an unexposed config with the value of ConntrackZone+2
	OVNMasqConntrackZone int
	// HostNodePortCTZone is an unexposed config with the value of ConntrackZone+3
	HostNodePortConntrackZone int
	// ReassemblyConntrackZone is an unexposed config with the value of ConntrackZone+4
	ReassemblyConntrackZone int
	// EncapType value defines the encapsulation protocol to use to transmit packets between
	// hypervisors. By default the value is 'geneve'
	EncapType string `gcfg:"encap-type"`
	// The IP address of the encapsulation endpoint. If not specified, the IP address the
	// NodeName resolves to will be used
	EncapIP string `gcfg:"encap-ip"`
	// The UDP Port of the encapsulation endpoint. If not specified, the IP default port
	// of 6081 will be used
	EncapPort uint `gcfg:"encap-port"`
	// Maximum number of milliseconds of idle time on connection that
	// ovn-controller waits before it will send a connection health probe.
	InactivityProbe int `gcfg:"inactivity-probe"`
	// Maximum number of seconds of idle time on the OpenFlow connection
	// that ovn-controller will wait before it sends a connection health probe
	OpenFlowProbe int `gcfg:"openflow-probe"`
	// Maximum number of milliseconds that ovn-controller waits before clearing existing flows
	// during start up, to make sure the initial flow compute is complete and avoid data plane
	// interruptions.
	OfctrlWaitBeforeClear int `gcfg:"ofctrl-wait-before-clear"`
	// The  boolean  flag  indicates  if  ovn-controller  should monitor all data in SB DB
	// instead of conditionally monitoring the data relevant to this node only.
	// By default monitor-all is enabled.
	MonitorAll bool `gcfg:"monitor-all"`
	// The  boolean  flag  indicates  if  ovn-controller  should
	// enable/disable the logical flow in-memory cache  it  uses
	// when processing Southbound database logical flow changes.
	// By default caching is enabled.
	LFlowCacheEnable bool `gcfg:"enable-lflow-cache"`
	// Maximum  number  of logical flow cache entries ovn-controller
	// may create when the logical flow  cache  is  enabled.  By
	// default the size of the cache is unlimited.
	LFlowCacheLimit uint `gcfg:"lflow-cache-limit"`
	// Maximum  number  of logical flow cache entries ovn-controller
	// may create when the logical flow  cache  is  enabled.  By
	// default the size of the cache is unlimited.
	LFlowCacheLimitKb uint `gcfg:"lflow-cache-limit-kb"`
	// RawClusterSubnets holds the unparsed cluster subnets. Should only be
	// used inside config module.
	RawClusterSubnets string `gcfg:"cluster-subnets"`
	// ClusterSubnets holds parsed cluster subnet entries and may be used
	// outside the config module.
	ClusterSubnets []CIDRNetworkEntry
	// EnableUDPAggregation is true if ovn-kubernetes should use UDP Generic Receive
	// Offload forwarding to improve the performance of containers that transmit lots
	// of small UDP packets by allowing them to be aggregated before passing through
	// the kernel network stack. This requires a new-enough kernel (5.15 or RHEL 8.5).
	EnableUDPAggregation bool `gcfg:"enable-udp-aggregation"`

	// Zone name to which ovnkube-node/ovnkube-controller belongs to
	Zone string `gcfg:"zone"`
}

// LoggingConfig holds logging-related parsed config file parameters and command-line overrides
type LoggingConfig struct {
	// File is the path of the file to log to
	File string `gcfg:"logfile"`
	// CNIFile is the path of the file for the CNI shim to log to
	CNIFile string `gcfg:"cnilogfile"`
	// LibovsdbFile is the path of the file for the libovsdb client to log to
	LibovsdbFile string `gcfg:"libovsdblogfile"`
	// Level is the logging verbosity level
	Level int `gcfg:"loglevel"`
	// LogFileMaxSize is the maximum size in megabytes of the logfile
	// before it gets rolled.
	LogFileMaxSize int `gcfg:"logfile-maxsize"`
	// LogFileMaxBackups represents the the maximum number of old log files to retain
	LogFileMaxBackups int `gcfg:"logfile-maxbackups"`
	// LogFileMaxAge represents the maximum number of days to retain old log files
	LogFileMaxAge int `gcfg:"logfile-maxage"`
	// Logging rate-limiting meter
	ACLLoggingRateLimit int `gcfg:"acl-logging-rate-limit"`
}

// MonitoringConfig holds monitoring-related parsed config file parameters and command-line overrides
type MonitoringConfig struct {
	// RawNetFlowTargets holds the unparsed NetFlow targets. Should only be used inside the config module.
	RawNetFlowTargets string `gcfg:"netflow-targets"`
	// RawSFlowTargets holds the unparsed SFlow targets. Should only be used inside the config module.
	RawSFlowTargets string `gcfg:"sflow-targets"`
	// RawIPFIXTargets holds the unparsed IPFIX targets. Should only be used inside the config module.
	RawIPFIXTargets string `gcfg:"ipfix-targets"`
	// NetFlowTargets holds the parsed NetFlow targets and may be used outside the config module.
	NetFlowTargets []HostPort
	// SFlowTargets holds the parsed SFlow targets and may be used outside the config module.
	SFlowTargets []HostPort
	// IPFIXTargets holds the parsed IPFIX targets and may be used outside the config module.
	IPFIXTargets []HostPort
}

// IPFIXConfig holds IPFIX-related performance configuration options. It requires that the ipfix-targets
// value of the [monitoring] section contains at least one endpoint.
type IPFIXConfig struct {
	// Sampling is an optional integer in range 1 to 4,294,967,295. It holds the rate at which
	// packets should be sampled and sent to each target collector. If not specified, defaults to
	// 400, which means one out of 400 packets, on average, will be sent to each target collector.
	Sampling uint `gcfg:"sampling"`
	// CacheActiveTimeout is an optional integer in range 0 to 4,200. It holds the maximum period in
	// seconds for which an IPFIX flow record is cached and aggregated before being sent. If not
	// specified, defaults to 60. If 0, caching is disabled.
	CacheActiveTimeout uint `gcfg:"cache-active-timeout"`
	// CacheMaxFlows is an optional integer in range 0 to 4,294,967,295. It holds the maximum number
	// of IPFIX flow records that can be cached at a time. If not specified in OVS, defaults to 0
	// (however, this controller defaults it to 60). If 0, caching is disabled.
	CacheMaxFlows uint `gcfg:"cache-max-flows"`
}

// CNIConfig holds CNI-related parsed config file parameters and command-line overrides
type CNIConfig struct {
	// ConfDir specifies the CNI config directory in which to write the overlay CNI config file
	ConfDir string `gcfg:"conf-dir"`
	// Plugin specifies the name of the CNI plugin
	Plugin string `gcfg:"plugin"`
}

// KubernetesConfig holds Kubernetes-related parsed config file parameters and command-line overrides
type KubernetesConfig struct {
	BootstrapKubeconfig     string        `gcfg:"bootstrap-kubeconfig"`
	CertDir                 string        `gcfg:"cert-dir"`
	CertDuration            time.Duration `gcfg:"cert-duration"`
	Kubeconfig              string        `gcfg:"kubeconfig"`
	CACert                  string        `gcfg:"cacert"`
	CAData                  []byte
	APIServer               string `gcfg:"apiserver"`
	Token                   string `gcfg:"token"`
	TokenFile               string `gcfg:"tokenFile"`
	CompatServiceCIDR       string `gcfg:"service-cidr"`
	RawServiceCIDRs         string `gcfg:"service-cidrs"`
	ServiceCIDRs            []*net.IPNet
	OVNConfigNamespace      string `gcfg:"ovn-config-namespace"`
	OVNEmptyLbEvents        bool   `gcfg:"ovn-empty-lb-events"`
	PodIP                   string `gcfg:"pod-ip"` // UNUSED
	RawNoHostSubnetNodes    string `gcfg:"no-hostsubnet-nodes"`
	NoHostSubnetNodes       labels.Selector
	HostNetworkNamespace    string `gcfg:"host-network-namespace"`
	DisableRequestedChassis bool   `gcfg:"disable-requestedchassis"`
	PlatformType            string `gcfg:"platform-type"`
	HealthzBindAddress      string `gcfg:"healthz-bind-address"`

	// CompatMetricsBindAddress is overridden by the corresponding option in MetricsConfig
	CompatMetricsBindAddress string `gcfg:"metrics-bind-address"`
	// CompatOVNMetricsBindAddress is overridden by the corresponding option in MetricsConfig
	CompatOVNMetricsBindAddress string `gcfg:"ovn-metrics-bind-address"`
	// CompatMetricsEnablePprof is overridden by the corresponding option in MetricsConfig
	CompatMetricsEnablePprof bool `gcfg:"metrics-enable-pprof"`

	DNSServiceNamespace string `gcfg:"dns-service-namespace"`
	DNSServiceName      string `gcfg:"dns-service-name"`
}

// MetricsConfig holds Prometheus metrics-related parameters.
type MetricsConfig struct {
	BindAddress           string `gcfg:"bind-address"`
	OVNMetricsBindAddress string `gcfg:"ovn-metrics-bind-address"`
	ExportOVSMetrics      bool   `gcfg:"export-ovs-metrics"`
	EnablePprof           bool   `gcfg:"enable-pprof"`
	NodeServerPrivKey     string `gcfg:"node-server-privkey"`
	NodeServerCert        string `gcfg:"node-server-cert"`
	// EnableConfigDuration holds the boolean flag to enable OVN-Kubernetes master to monitor OVN-Kubernetes master
	// configuration duration and optionally, its application to all nodes
	EnableConfigDuration bool `gcfg:"enable-config-duration"`
	EnableScaleMetrics   bool `gcfg:"enable-scale-metrics"`
}

// OVNKubernetesFeatureConfig holds OVN-Kubernetes feature enhancement config file parameters and command-line overrides
type OVNKubernetesFeatureConfig struct {
	// Admin Network Policy feature is enabled
	EnableAdminNetworkPolicy bool `gcfg:"enable-admin-network-policy"`
	// EgressIP feature is enabled
	EnableEgressIP bool `gcfg:"enable-egress-ip"`
	// EgressIP node reachability total timeout in seconds
	EgressIPReachabiltyTotalTimeout int  `gcfg:"egressip-reachability-total-timeout"`
	EnableEgressFirewall            bool `gcfg:"enable-egress-firewall"`
	EnableEgressQoS                 bool `gcfg:"enable-egress-qos"`
	EnableEgressService             bool `gcfg:"enable-egress-service"`
	EgressIPNodeHealthCheckPort     int  `gcfg:"egressip-node-healthcheck-port"`
	EnableMultiNetwork              bool `gcfg:"enable-multi-network"`
	EnableMultiNetworkPolicy        bool `gcfg:"enable-multi-networkpolicy"`
	EnableStatelessNetPol           bool `gcfg:"enable-stateless-netpol"`
	EnableInterconnect              bool `gcfg:"enable-interconnect"`
	EnableMultiExternalGateway      bool `gcfg:"enable-multi-external-gateway"`
	EnablePersistentIPs             bool `gcfg:"enable-persistent-ips"`
	EnableDNSNameResolver           bool `gcfg:"enable-dns-name-resolver"`
	EnableServiceTemplateSupport    bool `gcfg:"enable-svc-template-support"`
}

// GatewayMode holds the node gateway mode
type GatewayMode string

const (
	// GatewayModeDisabled indicates the node gateway mode is disabled
	GatewayModeDisabled GatewayMode = ""
	// GatewayModeShared indicates OVN shares a gateway interface with the node
	GatewayModeShared GatewayMode = "shared"
	// GatewayModeLocal indicates OVN creates a local NAT-ed interface for the gateway
	GatewayModeLocal GatewayMode = "local"
)

// GatewayConfig holds node gateway-related parsed config file parameters and command-line overrides
type GatewayConfig struct {
	// Mode is the gateway mode; if may be either empty (disabled), "shared", or "local"
	Mode GatewayMode `gcfg:"mode"`
	// Interface is the network interface to use for the gateway in "shared" mode
	Interface string `gcfg:"interface"`
	// Exgress gateway interface is the optional network interface to use for external gw pods traffic.
	EgressGWInterface string `gcfg:"egw-interface"`
	// NextHop is the gateway IP address of Interface; will be autodetected if not given
	NextHop string `gcfg:"next-hop"`
	// VLANID is the option VLAN tag to apply to gateway traffic for "shared" mode
	VLANID uint `gcfg:"vlan-id"`
	// NodeportEnable sets whether to provide Kubernetes NodePort service or not
	NodeportEnable bool `gcfg:"nodeport"`
	// DisableSNATMultipleGws sets whether to disable SNAT of egress traffic in namespaces annotated with routing-external-gws
	DisableSNATMultipleGWs bool `gcfg:"disable-snat-multiple-gws"`
	// V4JoinSubnet to be used in the cluster
	V4JoinSubnet string `gcfg:"v4-join-subnet"`
	// V6JoinSubnet to be used in the cluster
	V6JoinSubnet string `gcfg:"v6-join-subnet"`
	// V4MasqueradeSubnet to be used in the cluster
	V4MasqueradeSubnet string `gcfg:"v4-masquerade-subnet"`
	// V6MasqueradeSubnet to be used in the cluster
	V6MasqueradeSubnet string `gcfg:"v6-masquerade-subnet"`
	// MasqueradeIps to be allocated from the masquerade subnets to enable host to service traffic
	MasqueradeIPs MasqueradeIPsConfig

	// DisablePacketMTUCheck disables adding openflow flows to check packets too large to be
	// delivered to OVN due to pod MTU being lower than NIC MTU. Disabling this check will result in southbound packets
	// exceeding pod MTU to be dropped by OVN. With this check enabled, ICMP needs frag/packet too big will be sent
	// back to the original client
	DisablePacketMTUCheck bool `gcfg:"disable-pkt-mtu-check"`
	// RouterSubnet is the subnet to be used for the GR external port. auto-detected if not given.
	// Must match the the kube node IP address. Currently valid for DPU only.
	RouterSubnet string `gcfg:"router-subnet"`
	// SingeNode indicates the cluster has only one node
	SingleNode bool `gcfg:"single-node"`
	// DisableForwarding (enabled by default) controls if forwarding is allowed on OVNK controlled interfaces
	DisableForwarding bool `gcfg:"disable-forwarding"`
	// AllowNoUplink (disabled by default) controls if the external gateway bridge without an uplink port is allowed in local gateway mode.
	AllowNoUplink bool `gcfg:"allow-no-uplink"`
}

// OvnAuthConfig holds client authentication and location details for
// an OVN database (either northbound or southbound)
type OvnAuthConfig struct {
	// e.g: "ssl:192.168.1.2:6641,ssl:192.168.1.2:6642"
	Address        string `gcfg:"address"`
	PrivKey        string `gcfg:"client-privkey"`
	Cert           string `gcfg:"client-cert"`
	CACert         string `gcfg:"client-cacert"`
	CertCommonName string `gcfg:"cert-common-name"`
	Scheme         OvnDBScheme
	ElectionTimer  uint `gcfg:"election-timer"`
	northbound     bool

	exec kexec.Interface
}

// HAConfig holds configuration for HA
// configuration.
type HAConfig struct {
	ElectionLeaseDuration int `gcfg:"election-lease-duration"`
	ElectionRenewDeadline int `gcfg:"election-renew-deadline"`
	ElectionRetryPeriod   int `gcfg:"election-retry-period"`
}

// HybridOverlayConfig holds configuration for hybrid overlay
// configuration.
type HybridOverlayConfig struct {
	// Enabled indicates whether hybrid overlay features are enabled or not.
	Enabled bool `gcfg:"enabled"`
	// RawClusterSubnets holds the unparsed hybrid overlay cluster subnets.
	// Should only be used inside config module.
	RawClusterSubnets string `gcfg:"cluster-subnets"`
	// ClusterSubnets holds parsed hybrid overlay cluster subnet entries and
	// may be used outside the config module.
	ClusterSubnets []CIDRNetworkEntry
	// VXLANPort holds the VXLAN tunnel UDP port number.
	VXLANPort uint `gcfg:"hybrid-overlay-vxlan-port"`
}

// OvnKubeNodeConfig holds ovnkube-node configurations
type OvnKubeNodeConfig struct {
	Mode                   string `gcfg:"mode"`
	DPResourceDeviceIdsMap map[string][]string
	MgmtPortNetdev         string `gcfg:"mgmt-port-netdev"`
	MgmtPortDPResourceName string `gcfg:"mgmt-port-dp-resource-name"`
}

// ClusterManagerConfig holds configuration for ovnkube-cluster-manager
type ClusterManagerConfig struct {
	// V4TransitSwitchSubnet to be used in the cluster for interconnecting multiple zones
	V4TransitSwitchSubnet string `gcfg:"v4-transit-switch-subnet"`
	// V6TransitSwitchSubnet to be used in the cluster for interconnecting multiple zones
	V6TransitSwitchSubnet string `gcfg:"v6-transit-switch-subnet"`
}

// OvnDBScheme describes the OVN database connection transport method
type OvnDBScheme string

const (
	// OvnDBSchemeSSL specifies SSL as the OVN database transport method
	OvnDBSchemeSSL OvnDBScheme = "ssl"
	// OvnDBSchemeTCP specifies TCP as the OVN database transport method
	OvnDBSchemeTCP OvnDBScheme = "tcp"
	// OvnDBSchemeUnix specifies Unix domains sockets as the OVN database transport method
	OvnDBSchemeUnix OvnDBScheme = "unix"
)

// Config is used to read the structured config file and to cache config in testcases
type config struct {
	Default              DefaultConfig
	Logging              LoggingConfig
	Monitoring           MonitoringConfig
	IPFIX                IPFIXConfig
	CNI                  CNIConfig
	OVNKubernetesFeature OVNKubernetesFeatureConfig
	Kubernetes           KubernetesConfig
	Metrics              MetricsConfig
	OvnNorth             OvnAuthConfig
	OvnSouth             OvnAuthConfig
	Gateway              GatewayConfig
	MasterHA             HAConfig
	ClusterMgrHA         HAConfig
	HybridOverlay        HybridOverlayConfig
	OvnKubeNode          OvnKubeNodeConfig
	ClusterManager       ClusterManagerConfig
}

var (
	savedDefault              DefaultConfig
	savedLogging              LoggingConfig
	savedMonitoring           MonitoringConfig
	savedIPFIX                IPFIXConfig
	savedCNI                  CNIConfig
	savedOVNKubernetesFeature OVNKubernetesFeatureConfig
	savedKubernetes           KubernetesConfig
	savedMetrics              MetricsConfig
	savedOvnNorth             OvnAuthConfig
	savedOvnSouth             OvnAuthConfig
	savedGateway              GatewayConfig
	savedMasterHA             HAConfig
	savedClusterMgrHA         HAConfig
	savedHybridOverlay        HybridOverlayConfig
	savedOvnKubeNode          OvnKubeNodeConfig
	savedClusterManager       ClusterManagerConfig

	// legacy service-cluster-ip-range CLI option
	serviceClusterIPRange string
	// legacy cluster-subnet CLI option
	clusterSubnet string
	// legacy init-gateways CLI option
	initGateways bool
	// legacy gateway-local CLI option
	gatewayLocal bool
	// legacy disable-ovn-iface-id-ver CLI option
	disableOVNIfaceIDVer bool
)

func init() {
	// Cache original default config values
	savedDefault = Default
	savedLogging = Logging
	savedMonitoring = Monitoring
	savedIPFIX = IPFIX
	savedCNI = CNI
	savedOVNKubernetesFeature = OVNKubernetesFeature
	savedKubernetes = Kubernetes
	savedMetrics = Metrics
	savedOvnNorth = OvnNorth
	savedOvnSouth = OvnSouth
	savedGateway = Gateway
	savedMasterHA = MasterHA
	savedClusterMgrHA = ClusterMgrHA
	savedHybridOverlay = HybridOverlay
	savedOvnKubeNode = OvnKubeNode
	savedClusterManager = ClusterManager
	cli.VersionPrinter = func(c *cli.Context) {
		fmt.Printf("Version: %s\n", Version)
		fmt.Printf("Git commit: %s\n", Commit)
		fmt.Printf("Git branch: %s\n", Branch)
		fmt.Printf("Go version: %s\n", GoVersion)
		fmt.Printf("Build date: %s\n", BuildDate)
		fmt.Printf("OS/Arch: %s\n", OSArch)
	}
	Flags = GetFlags([]cli.Flag{})
}

// PrepareTestConfig restores default config values. Used by testcases to
// provide a pristine environment between tests.
func PrepareTestConfig() error {
	Default = savedDefault
	Logging = savedLogging
	Logging.Level = 5
	Monitoring = savedMonitoring
	IPFIX = savedIPFIX
	CNI = savedCNI
	OVNKubernetesFeature = savedOVNKubernetesFeature
	Kubernetes = savedKubernetes
	Metrics = savedMetrics
	OvnNorth = savedOvnNorth
	OvnSouth = savedOvnSouth
	Gateway = savedGateway
	MasterHA = savedMasterHA
	HybridOverlay = savedHybridOverlay
	OvnKubeNode = savedOvnKubeNode
	ClusterManager = savedClusterManager
	Kubernetes.DisableRequestedChassis = false
	EnableMulticast = false

	if err := completeConfig(); err != nil {
		return err
	}

	// Don't pick up defaults from the environment
	os.Unsetenv("KUBECONFIG")
	os.Unsetenv("K8S_CACERT")
	os.Unsetenv("K8S_APISERVER")
	os.Unsetenv("K8S_TOKEN")
	os.Unsetenv("K8S_TOKEN_FILE")

	return nil
}

// copy members of struct 'src' into the corresponding field in struct 'dst'
// if the field in 'src' is a non-zero int or a non-zero-length string and
// does not contain a default value. This function should be called with pointers to structs.
func overrideFields(dst, src, defaults interface{}) error {
	dstStruct := reflect.ValueOf(dst).Elem()
	srcStruct := reflect.ValueOf(src).Elem()
	if dstStruct.Kind() != srcStruct.Kind() || dstStruct.Kind() != reflect.Struct {
		return fmt.Errorf("mismatched value types")
	}
	if dstStruct.NumField() != srcStruct.NumField() {
		return fmt.Errorf("mismatched struct types")
	}

	var defStruct reflect.Value
	if defaults != nil {
		defStruct = reflect.ValueOf(defaults).Elem()
	}
	// Iterate over each field in dst/src Type so we can get the tags,
	// and use the field name to retrieve the field's actual value from
	// the dst/src instance
	var handled bool
	dstType := reflect.TypeOf(dst).Elem()
	for i := 0; i < dstType.NumField(); i++ {
		structField := dstType.Field(i)
		// Ignore private internal fields; we only care about overriding
		// 'gcfg' tagged fields read from CLI or the config file
		if _, ok := structField.Tag.Lookup("gcfg"); !ok {
			continue
		}
		handled = true

		dstField := dstStruct.FieldByName(structField.Name)
		srcField := srcStruct.FieldByName(structField.Name)
		var dv reflect.Value
		if defStruct.IsValid() {
			dv = defStruct.FieldByName(structField.Name)
		}
		if !dstField.IsValid() || !srcField.IsValid() {
			return fmt.Errorf("invalid struct %q field %q", dstType.Name(), structField.Name)
		}
		if dstField.Kind() != srcField.Kind() {
			return fmt.Errorf("mismatched struct %q fields %q", dstType.Name(), structField.Name)
		}
		if dv.IsValid() && reflect.DeepEqual(dv.Interface(), srcField.Interface()) {
			continue
		}
		dstField.Set(srcField)
	}
	if !handled {
		// No tags found in the struct so we don't know how to override
		return fmt.Errorf("failed to find 'gcfg' tags in struct %q", dstType.Name())
	}

	return nil
}

var cliConfig config

// CommonFlags capture general options.
var CommonFlags = []cli.Flag{
	// Mode flags
	&cli.StringFlag{
		Name:  "init-master",
		Usage: "initialize master (both cluster-manager and ovnkube-controller), requires the hostname as argument",
	},
	&cli.StringFlag{
		Name:  "init-cluster-manager",
		Usage: "initialize cluster manager (but not ovnkube-controller), requires the hostname as argument",
	},
	&cli.StringFlag{
		Name:  "init-ovnkube-controller",
		Usage: "initialize ovnkube-controller (but not cluster-manager), requires the hostname as argument",
	},
	&cli.StringFlag{
		Name:  "init-node",
		Usage: "initialize node, requires the name that node is registered with in kubernetes cluster",
	},
	&cli.StringFlag{
		Name:  "cleanup-node",
		Usage: "cleanup node, requires the name that node is registered with in kubernetes cluster",
	},
	&cli.StringFlag{
		Name:  "pidfile",
		Usage: "Name of file that will hold the ovnkube pid (optional)",
	},
	&cli.StringFlag{
		Name:  "config-file",
		Usage: "configuration file path (default: /etc/openvswitch/ovn_k8s.conf)",
		//Value: "/etc/openvswitch/ovn_k8s.conf",
	},
	&cli.IntFlag{
		Name:        "mtu",
		Usage:       "MTU value used for the overlay networks (default: 1400)",
		Destination: &cliConfig.Default.MTU,
		Value:       Default.MTU,
	},
	&cli.IntFlag{
		Name:        "routable-mtu",
		Usage:       "Maximum routable MTU between nodes, used to facilitate an MTU migration procedure where different nodes might be using different MTU values",
		Destination: &cliConfig.Default.RoutableMTU,
	},
	&cli.IntFlag{
		Name:        "conntrack-zone",
		Usage:       "For gateway nodes, the conntrack zone used for conntrack flow rules (default: 64000)",
		Destination: &cliConfig.Default.ConntrackZone,
		Value:       Default.ConntrackZone,
	},
	&cli.StringFlag{
		Name:        "encap-type",
		Usage:       "The encapsulation protocol to use to transmit packets between hypervisors (default: geneve)",
		Destination: &cliConfig.Default.EncapType,
		Value:       Default.EncapType,
	},
	&cli.StringFlag{
		Name:        "encap-ip",
		Usage:       "The IP address of the encapsulation endpoint (default: Node IP address resolved from Node hostname)",
		Destination: &cliConfig.Default.EncapIP,
	},
	&cli.UintFlag{
		Name:        "encap-port",
		Usage:       "The UDP port used by the encapsulation endpoint (default: 6081)",
		Destination: &cliConfig.Default.EncapPort,
		Value:       Default.EncapPort,
	},
	&cli.IntFlag{
		Name: "inactivity-probe",
		Usage: "Maximum number of milliseconds of idle time on " +
			"connection for ovn-controller before it sends a inactivity probe",
		Destination: &cliConfig.Default.InactivityProbe,
		Value:       Default.InactivityProbe,
	},
	&cli.IntFlag{
		Name: "openflow-probe",
		Usage: "Maximum number of seconds of idle time on the openflow " +
			"connection for ovn-controller before it sends a inactivity probe",
		Destination: &cliConfig.Default.OpenFlowProbe,
		Value:       Default.OpenFlowProbe,
	},
	&cli.IntFlag{
		Name: "ofctrl-wait-before-clear",
		Usage: "Maximum number of milliseconds that ovn-controller waits before " +
			"clearing existing flows during start up, to make sure the initial flow " +
			"compute is complete and avoid data plane interruptions.",
		Destination: &cliConfig.Default.OfctrlWaitBeforeClear,
		Value:       Default.OfctrlWaitBeforeClear,
	},
	&cli.BoolFlag{
		Name: "monitor-all",
		Usage: "Enable monitoring all data from SB DB instead of conditionally " +
			"monitoring the data relevant to this node only. " +
			"By default it is enabled.",
		Destination: &cliConfig.Default.MonitorAll,
		Value:       Default.MonitorAll,
	},
	&cli.BoolFlag{
		Name: "enable-lflow-cache",
		Usage: "Enable the logical flow in-memory cache it uses " +
			"when processing Southbound database logical flow changes. " +
			"By default caching is enabled.",
		Destination: &cliConfig.Default.LFlowCacheEnable,
		Value:       Default.LFlowCacheEnable,
	},
	&cli.UintFlag{
		Name: "lflow-cache-limit",
		Usage: "Maximum number of logical flow cache entries ovn-controller " +
			"may create when the logical flow cache is enabled. By " +
			"default the size of the cache is unlimited.",
		Destination: &cliConfig.Default.LFlowCacheLimit,
		Value:       Default.LFlowCacheLimit,
	},
	&cli.UintFlag{
		Name: "lflow-cache-limit-kb",
		Usage: "Maximum size of the logical flow cache ovn-controller " +
			"may create when the logical flow cache is enabled. By " +
			"default the size of the cache is unlimited.",
		Destination: &cliConfig.Default.LFlowCacheLimitKb,
		Value:       Default.LFlowCacheLimitKb,
	},
	&cli.StringFlag{
		Name:        "cluster-subnet",
		Usage:       "Deprecated alias for cluster-subnets.",
		Destination: &clusterSubnet,
	},
	&cli.StringFlag{
		Name:  "cluster-subnets",
		Value: Default.RawClusterSubnets,
		Usage: "A comma separated set of IP subnets and the associated " +
			"hostsubnet prefix lengths to use for the cluster (eg, \"10.128.0.0/14/23,10.0.0.0/14/23\"). " +
			"Each entry is given in the form [IP address/prefix-length/hostsubnet-prefix-length] " +
			"and cannot overlap with other entries. The hostsubnet-prefix-length " +
			"defines how large a subnet is given to each node and may be different " +
			"for each entry. For IPv6 subnets, it must be 64 (and does not need to " +
			"be explicitly specified). For IPv4 subnets an explicit " +
			"hostsubnet-prefix should be specified, but for backward compatibility " +
			"it defaults to 24 if unspecified.",
		Destination: &cliConfig.Default.RawClusterSubnets,
	},
	&cli.BoolFlag{
		Name:        "unprivileged-mode",
		Usage:       "Run ovnkube-node container in unprivileged mode. Valid only with --init-node option.",
		Destination: &UnprivilegedMode,
	},
	&cli.BoolFlag{
		Name:        "enable-multicast",
		Usage:       "Adds multicast support. Valid only with --init-master option.",
		Destination: &EnableMulticast,
	},
	// Logging options
	&cli.IntFlag{
		Name:        "loglevel",
		Usage:       "log verbosity and level: info, warn, fatal, error are always printed no matter the log level. Use 5 for debug (default: 4)",
		Destination: &cliConfig.Logging.Level,
		Value:       Logging.Level,
	},
	&cli.StringFlag{
		Name:        "logfile",
		Usage:       "path of a file to direct log output to",
		Destination: &cliConfig.Logging.File,
	},
	&cli.StringFlag{
		Name:        "cnilogfile",
		Usage:       "path of a file to direct log from cni shim to output to (default: /var/log/ovn-kubernetes/ovn-k8s-cni-overlay.log)",
		Destination: &cliConfig.Logging.CNIFile,
		Value:       "/var/log/ovn-kubernetes/ovn-k8s-cni-overlay.log",
	},
	&cli.StringFlag{
		Name:        "libovsdblogfile",
		Usage:       "path of a file to direct log from libovsdb client to output to (default is to use same as --logfile)",
		Destination: &cliConfig.Logging.LibovsdbFile,
	},
	// Logfile rotation parameters
	&cli.IntFlag{
		Name:        "logfile-maxsize",
		Usage:       "Maximum size in bytes of the log file before it gets rolled",
		Destination: &cliConfig.Logging.LogFileMaxSize,
		Value:       Logging.LogFileMaxSize,
	},
	&cli.IntFlag{
		Name:        "logfile-maxbackups",
		Usage:       "Maximum number of old log files to retain",
		Destination: &cliConfig.Logging.LogFileMaxBackups,
		Value:       Logging.LogFileMaxBackups,
	},
	&cli.IntFlag{
		Name:        "logfile-maxage",
		Usage:       "Maximum number of days to retain old log files",
		Destination: &cliConfig.Logging.LogFileMaxAge,
		Value:       Logging.LogFileMaxAge,
	},
	&cli.IntFlag{
		Name:        "acl-logging-rate-limit",
		Usage:       "The largest number of messages per second that gets logged before drop (default 20)",
		Destination: &cliConfig.Logging.ACLLoggingRateLimit,
		Value:       20,
	},
	&cli.StringFlag{
		Name:        "zone",
		Usage:       "zone name to which ovnkube-node/ovnkube-controller belongs to",
		Value:       Default.Zone,
		Destination: &cliConfig.Default.Zone,
	},
}

// MonitoringFlags capture monitoring-related options
var MonitoringFlags = []cli.Flag{
	// Monitoring options
	&cli.StringFlag{
		Name:  "netflow-targets",
		Value: Monitoring.RawNetFlowTargets,
		Usage: "A comma separated set of NetFlow collectors to export flow data (eg, \"10.128.0.150:2056,10.0.0.151:2056\")." +
			"Each entry is given in the form [IP address:port] or [:port]. If only port is provided, it uses the Node IP",
		Destination: &cliConfig.Monitoring.RawNetFlowTargets,
	},
	&cli.StringFlag{
		Name:  "sflow-targets",
		Value: Monitoring.RawSFlowTargets,
		Usage: "A comma separated set of SFlow collectors to export flow data (eg, \"10.128.0.150:6343,10.0.0.151:6343\")." +
			"Each entry is given in the form [IP address:port] or [:port]. If only port is provided, it uses the Node IP",
		Destination: &cliConfig.Monitoring.RawSFlowTargets,
	},
	&cli.StringFlag{
		Name:  "ipfix-targets",
		Value: Monitoring.RawIPFIXTargets,
		Usage: "A comma separated set of IPFIX collectors to export flow data (eg, \"10.128.0.150:2055,10.0.0.151:2055\")." +
			"Each entry is given in the form [IP address:port] or [:port]. If only port is provided, it uses the Node IP",
		Destination: &cliConfig.Monitoring.RawIPFIXTargets,
	},
}

// IPFIXFlags capture IPFIX-related options
var IPFIXFlags = []cli.Flag{
	&cli.UintFlag{
		Name:        "ipfix-sampling",
		Usage:       "Rate at which packets should be sampled and sent to each target collector (default: 400)",
		Destination: &cliConfig.IPFIX.Sampling,
		Value:       IPFIX.Sampling,
	},
	&cli.UintFlag{
		Name:        "ipfix-cache-max-flows",
		Usage:       "Maximum number of IPFIX flow records that can be cached at a time. If 0, caching is disabled (default: 0)",
		Destination: &cliConfig.IPFIX.CacheMaxFlows,
		Value:       IPFIX.CacheMaxFlows,
	}, &cli.UintFlag{
		Name:        "ipfix-cache-active-timeout",
		Usage:       "Maximum period in seconds for which an IPFIX flow record is cached and aggregated before being sent. If 0, caching is disabled (default: 60)",
		Destination: &cliConfig.IPFIX.CacheActiveTimeout,
		Value:       IPFIX.CacheActiveTimeout,
	},
}

// CNIFlags capture CNI-related options
var CNIFlags = []cli.Flag{
	// CNI options
	&cli.StringFlag{
		Name:        "cni-conf-dir",
		Usage:       "the CNI config directory in which to write the overlay CNI config file (default: /etc/cni/net.d)",
		Destination: &cliConfig.CNI.ConfDir,
		Value:       CNI.ConfDir,
	},
	&cli.StringFlag{
		Name:        "cni-plugin",
		Usage:       "the name of the CNI plugin (default: ovn-k8s-cni-overlay)",
		Destination: &cliConfig.CNI.Plugin,
		Value:       CNI.Plugin,
	},
}

// OVNK8sFeatureFlags capture OVN-Kubernetes feature related options
var OVNK8sFeatureFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:        "enable-admin-network-policy",
		Usage:       "Configure to use Admin Network Policy CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableAdminNetworkPolicy,
		Value:       OVNKubernetesFeature.EnableAdminNetworkPolicy,
	},
	&cli.BoolFlag{
		Name:        "enable-egress-ip",
		Usage:       "Configure to use EgressIP CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableEgressIP,
		Value:       OVNKubernetesFeature.EnableEgressIP,
	},
	&cli.IntFlag{
		Name:        "egressip-reachability-total-timeout",
		Usage:       "EgressIP node reachability total timeout in seconds (default: 1)",
		Destination: &cliConfig.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout,
		Value:       1,
	},
	&cli.BoolFlag{
		Name:        "enable-egress-firewall",
		Usage:       "Configure to use EgressFirewall CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableEgressFirewall,
		Value:       OVNKubernetesFeature.EnableEgressFirewall,
	},
	&cli.BoolFlag{
		Name:        "enable-egress-qos",
		Usage:       "Configure to use EgressQoS CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableEgressQoS,
		Value:       OVNKubernetesFeature.EnableEgressQoS,
	},
	&cli.IntFlag{
		Name:        "egressip-node-healthcheck-port",
		Usage:       "Configure EgressIP node reachability using gRPC on this TCP port.",
		Destination: &cliConfig.OVNKubernetesFeature.EgressIPNodeHealthCheckPort,
	},
	&cli.BoolFlag{
		Name:        "enable-multi-network",
		Usage:       "Configure to use multiple NetworkAttachmentDefinition CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableMultiNetwork,
		Value:       OVNKubernetesFeature.EnableMultiNetwork,
	},
	&cli.BoolFlag{
		Name:        "enable-multi-networkpolicy",
		Usage:       "Configure to use MultiNetworkPolicy CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableMultiNetworkPolicy,
		Value:       OVNKubernetesFeature.EnableMultiNetworkPolicy,
	},
	&cli.BoolFlag{
		Name:        "enable-stateless-netpol",
		Usage:       "Configure to use stateless network policy feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableStatelessNetPol,
		Value:       OVNKubernetesFeature.EnableStatelessNetPol,
	},
	&cli.BoolFlag{
		Name:        "enable-interconnect",
		Usage:       "Configure to enable interconnecting multiple zones.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableInterconnect,
		Value:       OVNKubernetesFeature.EnableInterconnect,
	},
	&cli.BoolFlag{
		Name:        "enable-egress-service",
		Usage:       "Configure to use EgressService CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableEgressService,
		Value:       OVNKubernetesFeature.EnableEgressService,
	},
	&cli.BoolFlag{
		Name:        "enable-multi-external-gateway",
		Usage:       "Configure to use AdminPolicyBasedExternalRoute CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableMultiExternalGateway,
		Value:       OVNKubernetesFeature.EnableMultiExternalGateway,
	},
	&cli.BoolFlag{
		Name:        "enable-persistent-ips",
		Usage:       "Configure to use the persistent ips feature for virtualization with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnablePersistentIPs,
		Value:       OVNKubernetesFeature.EnablePersistentIPs,
	},
	&cli.BoolFlag{
		Name:        "enable-dns-name-resolver",
		Usage:       "Configure to use DNSNameResolver CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableDNSNameResolver,
		Value:       OVNKubernetesFeature.EnableDNSNameResolver,
	},
	&cli.BoolFlag{
		Name:        "enable-svc-template-support",
		Usage:       "Configure to use svc-template with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableServiceTemplateSupport,
		Value:       OVNKubernetesFeature.EnableServiceTemplateSupport,
	},
}

// K8sFlags capture Kubernetes-related options
var K8sFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "service-cluster-ip-range",
		Usage:       "Deprecated alias for k8s-service-cidrs.",
		Destination: &serviceClusterIPRange,
	},
	&cli.StringFlag{
		Name:        "k8s-service-cidr",
		Usage:       "Deprecated alias for k8s-service-cidrs.",
		Destination: &cliConfig.Kubernetes.CompatServiceCIDR,
	},
	&cli.StringFlag{
		Name: "k8s-service-cidrs",
		Usage: "A comma-separated set of CIDR notation IP ranges from which k8s assigns " +
			"service cluster IPs. This should be the same as the value " +
			"provided for kube-apiserver \"--service-cluster-ip-range\" " +
			"option. (default: 172.16.1.0/24)",
		Destination: &cliConfig.Kubernetes.RawServiceCIDRs,
		Value:       Kubernetes.RawServiceCIDRs,
	},
	&cli.StringFlag{
		Name:        "k8s-kubeconfig",
		Usage:       "absolute path to the Kubernetes kubeconfig file (not required if the --k8s-apiserver, --k8s-ca-cert, and --k8s-token are given)",
		Destination: &cliConfig.Kubernetes.Kubeconfig,
	},
	&cli.StringFlag{
		Name:        "bootstrap-kubeconfig",
		Usage:       "absolute path to the Kubernetes kubeconfig file that is used to create the initial, per node, client certificates (should only be used together with 'cert-dir')",
		Destination: &cliConfig.Kubernetes.BootstrapKubeconfig,
	},
	&cli.StringFlag{
		Name:        "k8s-apiserver",
		Usage:       "URL of the Kubernetes API server (not required if --k8s-kubeconfig is given) (default: http://localhost:8443)",
		Destination: &cliConfig.Kubernetes.APIServer,
		Value:       Kubernetes.APIServer,
	},
	&cli.StringFlag{
		Name:        "cert-dir",
		Usage:       "absolute path to the directory of the client key and certificate (not required if --k8s-kubeconfig or --k8s-apiserver, --k8s-ca-cert, and --k8s-token are given)",
		Destination: &cliConfig.Kubernetes.CertDir,
	},
	&cli.DurationFlag{
		Name:        "cert-duration",
		Usage:       "requested certificate duration, default: 10min",
		Destination: &cliConfig.Kubernetes.CertDuration,
		Value:       Kubernetes.CertDuration,
	},
	&cli.StringFlag{
		Name:        "k8s-cacert",
		Usage:       "the absolute path to the Kubernetes API CA certificate (not required if --k8s-kubeconfig is given)",
		Destination: &cliConfig.Kubernetes.CACert,
	},
	&cli.StringFlag{
		Name:        "k8s-token",
		Usage:       "the Kubernetes API authentication token (not required if --k8s-kubeconfig is given)",
		Destination: &cliConfig.Kubernetes.Token,
	},
	&cli.StringFlag{
		Name:        "k8s-token-file",
		Usage:       "the path to Kubernetes API token. If set, it is periodically read and takes precedence over k8s-token",
		Destination: &cliConfig.Kubernetes.TokenFile,
	},
	&cli.StringFlag{
		Name:        "ovn-config-namespace",
		Usage:       "specify a namespace which will contain services to config the OVN databases",
		Destination: &cliConfig.Kubernetes.OVNConfigNamespace,
		Value:       Kubernetes.OVNConfigNamespace,
	},
	&cli.BoolFlag{
		Name: "ovn-empty-lb-events",
		Usage: "If set, then load balancers do not get deleted when all backends are removed. " +
			"Instead, ovn-kubernetes monitors the OVN southbound database for empty lb backends " +
			"controller events. If one arrives, then a NeedPods event is sent so that Kubernetes " +
			"will spin up pods for the load balancer to send traffic to.",
		Destination: &cliConfig.Kubernetes.OVNEmptyLbEvents,
	},
	&cli.StringFlag{
		Name:  "pod-ip",
		Usage: "UNUSED",
	},
	&cli.StringFlag{
		Name:        "no-hostsubnet-nodes",
		Usage:       "Specify a label for nodes that will manage their own hostsubnets",
		Destination: &cliConfig.Kubernetes.RawNoHostSubnetNodes,
	},
	&cli.StringFlag{
		Name:        "host-network-namespace",
		Usage:       "specify a namespace which will be used to classify host network traffic for network policy",
		Destination: &cliConfig.Kubernetes.HostNetworkNamespace,
		Value:       Kubernetes.HostNetworkNamespace,
	},
	&cli.BoolFlag{
		Name:        "disable-requestedchassis",
		Usage:       "If set to true, requested-chassis option will not be set during pod creation",
		Destination: &cliConfig.Kubernetes.DisableRequestedChassis,
		Value:       Kubernetes.DisableRequestedChassis,
	},
	&cli.StringFlag{
		Name: "platform-type",
		Usage: "The cloud provider platform type ovn-kubernetes is deployed on. " +
			"Valid values can be found in: https://github.com/ovn-org/ovn-kubernetes/blob/master/go-controller/vendor/github.com/openshift/api/config/v1/types_infrastructure.go#L130-L172",
		Destination: &cliConfig.Kubernetes.PlatformType,
		Value:       Kubernetes.PlatformType,
	},
	&cli.StringFlag{
		Name:        "healthz-bind-address",
		Usage:       "The IP address and port for the node proxy healthz server to serve on (set to '0.0.0.0:10256' or '[::]:10256' for listening in all interfaces and IP families). Disabled by default.",
		Destination: &cliConfig.Kubernetes.HealthzBindAddress,
	},
	&cli.StringFlag{
		Name:        "dns-service-namespace",
		Usage:       "DNS kubernetes service namespace used to expose name resolving to live migratable vms.",
		Destination: &cliConfig.Kubernetes.DNSServiceNamespace,
		Value:       Kubernetes.DNSServiceNamespace,
	},
	&cli.StringFlag{
		Name:        "dns-service-name",
		Usage:       "DNS kubernetes service name used to expose name resolving to live migratable vms.",
		Destination: &cliConfig.Kubernetes.DNSServiceName,
		Value:       Kubernetes.DNSServiceName,
	},
}

// MetricsFlags capture metrics-related options
var MetricsFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "metrics-bind-address",
		Usage:       "The IP address and port for the OVN K8s metrics server to serve on (set to 0.0.0.0 for all IPv4 interfaces)",
		Destination: &cliConfig.Metrics.BindAddress,
	},
	&cli.StringFlag{
		Name:        "ovn-metrics-bind-address",
		Usage:       "The IP address and port for the OVN metrics server to serve on (set to 0.0.0.0 for all IPv4 interfaces)",
		Destination: &cliConfig.Metrics.OVNMetricsBindAddress,
	},
	&cli.BoolFlag{
		Name:        "export-ovs-metrics",
		Usage:       "When true exports OVS metrics from the OVN metrics server",
		Destination: &cliConfig.Metrics.ExportOVSMetrics,
	},
	&cli.BoolFlag{
		Name:        "metrics-enable-pprof",
		Usage:       "If true, then also accept pprof requests on the metrics port.",
		Destination: &cliConfig.Metrics.EnablePprof,
		Value:       Metrics.EnablePprof,
	},
	&cli.StringFlag{
		Name:        "node-server-privkey",
		Usage:       "Private key that the OVN node K8s metrics server uses to serve metrics over TLS.",
		Destination: &cliConfig.Metrics.NodeServerPrivKey,
	},
	&cli.StringFlag{
		Name:        "node-server-cert",
		Usage:       "Certificate that the OVN node K8s metrics server uses to serve metrics over TLS.",
		Destination: &cliConfig.Metrics.NodeServerCert,
	},
	&cli.BoolFlag{
		Name:        "metrics-enable-config-duration",
		Usage:       "Enables monitoring OVN-Kubernetes master and OVN configuration duration",
		Destination: &cliConfig.Metrics.EnableConfigDuration,
	},
	&cli.BoolFlag{
		Name:        "metrics-enable-scale",
		Usage:       "Enables metrics related to scaling",
		Destination: &cliConfig.Metrics.EnableScaleMetrics,
	},
}

// OvnNBFlags capture OVN northbound database options
var OvnNBFlags = []cli.Flag{
	&cli.StringFlag{
		Name: "nb-address",
		Usage: "IP address and port of the OVN northbound API " +
			"(eg, ssl:1.2.3.4:6641,ssl:1.2.3.5:6642).  Leave empty to " +
			"use a local unix socket.",
		Destination: &cliConfig.OvnNorth.Address,
	},
	&cli.StringFlag{
		Name: "nb-client-privkey",
		Usage: "Private key that the client should use for talking to the OVN database (default when ssl address is used: /etc/openvswitch/ovnnb-privkey.pem).  " +
			"Default value for this setting is empty which defaults to use local unix socket.",
		Destination: &cliConfig.OvnNorth.PrivKey,
	},
	&cli.StringFlag{
		Name: "nb-client-cert",
		Usage: "Client certificate that the client should use for talking to the OVN database (default when ssl address is used: /etc/openvswitch/ovnnb-cert.pem). " +
			"Default value for this setting is empty which defaults to use local unix socket.",
		Destination: &cliConfig.OvnNorth.Cert,
	},
	&cli.StringFlag{
		Name: "nb-client-cacert",
		Usage: "CA certificate that the client should use for talking to the OVN database (default when ssl address is used: /etc/openvswitch/ovnnb-ca.cert)." +
			"Default value for this setting is empty which defaults to use local unix socket.",
		Destination: &cliConfig.OvnNorth.CACert,
	},
	&cli.StringFlag{
		Name: "nb-cert-common-name",
		Usage: "Common Name of the certificate used for TLS server certificate verification. " +
			"In cases where the certificate doesn't have any SAN Extensions, this parameter " +
			"should match the DNS(hostname) of the server. In case the certificate has a " +
			"SAN extension, this parameter should match one of the SAN fields.",
		Destination: &cliConfig.OvnNorth.CertCommonName,
	},
	&cli.UintFlag{
		Name:        "nb-raft-election-timer",
		Usage:       "The desired northbound database election timer.",
		Destination: &cliConfig.OvnNorth.ElectionTimer,
	},
}

// OvnSBFlags capture OVN southbound database options
var OvnSBFlags = []cli.Flag{
	&cli.StringFlag{
		Name: "sb-address",
		Usage: "IP address and port of the OVN southbound API " +
			"(eg, ssl:1.2.3.4:6642,ssl:1.2.3.5:6642).  " +
			"Leave empty to use a local unix socket.",
		Destination: &cliConfig.OvnSouth.Address,
	},
	&cli.StringFlag{
		Name: "sb-client-privkey",
		Usage: "Private key that the client should use for talking to the OVN database (default when ssl address is used: /etc/openvswitch/ovnsb-privkey.pem)." +
			"Default value for this setting is empty which defaults to use local unix socket.",
		Destination: &cliConfig.OvnSouth.PrivKey,
	},
	&cli.StringFlag{
		Name: "sb-client-cert",
		Usage: "Client certificate that the client should use for talking to the OVN database(default when ssl address is used: /etc/openvswitch/ovnsb-cert.pem).  " +
			"Default value for this setting is empty which defaults to use local unix socket.",
		Destination: &cliConfig.OvnSouth.Cert,
	},
	&cli.StringFlag{
		Name: "sb-client-cacert",
		Usage: "CA certificate that the client should use for talking to the OVN database (default when ssl address is used /etc/openvswitch/ovnsb-ca.cert). " +
			"Default value for this setting is empty which defaults to use local unix socket.",
		Destination: &cliConfig.OvnSouth.CACert,
	},
	&cli.StringFlag{
		Name: "sb-cert-common-name",
		Usage: "Common Name of the certificate used for TLS server certificate verification. " +
			"In cases where the certificate doesn't have any SAN Extensions, this parameter " +
			"should match the DNS(hostname) of the server. In case the certificate has a " +
			"SAN extension, this parameter should match one of the SAN fields.",
		Destination: &cliConfig.OvnSouth.CertCommonName,
	},
	&cli.UintFlag{
		Name:        "sb-raft-election-timer",
		Usage:       "The desired southbound database election timer.",
		Destination: &cliConfig.OvnSouth.ElectionTimer,
	},
}

// OVNGatewayFlags capture L3 Gateway related flags
var OVNGatewayFlags = []cli.Flag{
	&cli.StringFlag{
		Name: "gateway-mode",
		Usage: "Sets the cluster gateway mode. One of \"shared\", " +
			"or \"local\". If not given, gateway functionality is disabled.",
	},
	&cli.StringFlag{
		Name: "gateway-interface",
		Usage: "The interface on nodes that will be the gateway interface. " +
			"If none specified, then the node's interface on which the " +
			"default gateway is configured will be used as the gateway " +
			"interface. Only useful with \"init-gateways\"",
		Destination: &cliConfig.Gateway.Interface,
	},
	&cli.StringFlag{
		Name: "exgw-interface",
		Usage: "The interface on nodes that will be used for external gw network traffic. " +
			"If none specified, ovnk will use the default interface",
		Destination: &cliConfig.Gateway.EgressGWInterface,
	},
	&cli.StringFlag{
		Name: "gateway-nexthop",
		Usage: "The external default gateway which is used as a next hop by " +
			"OVN gateway.  This is many times just the default gateway " +
			"of the node in question. If not specified, the default gateway" +
			"configured in the node is used. Only useful with " +
			"\"init-gateways\"",
		Destination: &cliConfig.Gateway.NextHop,
	},
	&cli.UintFlag{
		Name: "gateway-vlanid",
		Usage: "The VLAN on which the external network is available. " +
			"Valid only for Shared Gateway interface mode.",
		Destination: &cliConfig.Gateway.VLANID,
	},
	&cli.BoolFlag{
		Name:        "nodeport",
		Usage:       "Setup nodeport based ingress on gateways.",
		Destination: &cliConfig.Gateway.NodeportEnable,
	},
	&cli.BoolFlag{
		Name:        "disable-snat-multiple-gws",
		Usage:       "Disable SNAT for egress traffic with multiple gateways.",
		Destination: &cliConfig.Gateway.DisableSNATMultipleGWs,
	},
	&cli.BoolFlag{
		Name:        "disable-forwarding",
		Usage:       "Disable forwarding on OVNK controlled interfaces.",
		Destination: &cliConfig.Gateway.DisableForwarding,
	},
	&cli.StringFlag{
		Name:        "gateway-v4-join-subnet",
		Usage:       "The v4 join subnet used for assigning join switch IPv4 addresses",
		Destination: &cliConfig.Gateway.V4JoinSubnet,
		Value:       Gateway.V4JoinSubnet,
	},
	&cli.StringFlag{
		Name:        "gateway-v6-join-subnet",
		Usage:       "The v6 join subnet used for assigning join switch IPv6 addresses",
		Destination: &cliConfig.Gateway.V6JoinSubnet,
		Value:       Gateway.V6JoinSubnet,
	},
	&cli.StringFlag{
		Name:        "gateway-v4-masquerade-subnet",
		Usage:       "The v4 masquerade subnet used for assigning masquerade IPv4 addresses",
		Destination: &cliConfig.Gateway.V4MasqueradeSubnet,
		Value:       Gateway.V4MasqueradeSubnet,
	},
	&cli.StringFlag{
		Name:        "gateway-v6-masquerade-subnet",
		Usage:       "The v6 masquerade subnet used for assigning masquerade IPv6 addresses",
		Destination: &cliConfig.Gateway.V6MasqueradeSubnet,
		Value:       Gateway.V6MasqueradeSubnet,
	},
	&cli.BoolFlag{
		Name:        "disable-pkt-mtu-check",
		Usage:       "Disable OpenFlow checks for if packet size is greater than pod MTU",
		Destination: &cliConfig.Gateway.DisablePacketMTUCheck,
	},
	&cli.StringFlag{
		Name: "gateway-router-subnet",
		Usage: "The Subnet to be used for the gateway router external port (shared mode only). " +
			"auto-detected if not given. Must match the the kube node IP address. " +
			"Currently valid for DPUs only",
		Destination: &cliConfig.Gateway.RouterSubnet,
		Value:       Gateway.RouterSubnet,
	},
	&cli.BoolFlag{
		Name: "single-node",
		Usage: "Enable single node optimizations. " +
			"Single node indicates a one node cluster and allows to simplify ovn-kubernetes gateway logic",
		Destination: &cliConfig.Gateway.SingleNode,
	},
	&cli.BoolFlag{
		Name:        "allow-no-uplink",
		Usage:       "Allow the external gateway bridge without an uplink port in local gateway mode",
		Destination: &cliConfig.Gateway.AllowNoUplink,
	},
	// Deprecated CLI options
	&cli.BoolFlag{
		Name:        "init-gateways",
		Usage:       "DEPRECATED; use --gateway-mode instead",
		Destination: &initGateways,
	},
	&cli.BoolFlag{
		Name:        "gateway-local",
		Usage:       "DEPRECATED; use --gateway-mode instead",
		Destination: &gatewayLocal,
	},
}

// MasterHAFlags capture leader election flags for master
var MasterHAFlags = []cli.Flag{
	&cli.IntFlag{
		Name:        "ha-election-lease-duration",
		Usage:       "Leader election lease duration (in secs) (default: 60)",
		Destination: &cliConfig.MasterHA.ElectionLeaseDuration,
		Value:       MasterHA.ElectionLeaseDuration,
	},
	&cli.IntFlag{
		Name:        "ha-election-renew-deadline",
		Usage:       "Leader election renew deadline (in secs) (default: 30)",
		Destination: &cliConfig.MasterHA.ElectionRenewDeadline,
		Value:       MasterHA.ElectionRenewDeadline,
	},
	&cli.IntFlag{
		Name:        "ha-election-retry-period",
		Usage:       "Leader election retry period (in secs) (default: 20)",
		Destination: &cliConfig.MasterHA.ElectionRetryPeriod,
		Value:       MasterHA.ElectionRetryPeriod,
	},
}

// ClusterMgrHAFlags capture leader election flags for cluster manager
var ClusterMgrHAFlags = []cli.Flag{
	&cli.IntFlag{
		Name:        "cluster-manager-ha-election-lease-duration",
		Usage:       "Leader election lease duration (in secs) (default: 60)",
		Destination: &cliConfig.ClusterMgrHA.ElectionLeaseDuration,
		Value:       ClusterMgrHA.ElectionLeaseDuration,
	},
	&cli.IntFlag{
		Name:        "cluster-manager-ha-election-renew-deadline",
		Usage:       "Leader election renew deadline (in secs) (default: 30)",
		Destination: &cliConfig.ClusterMgrHA.ElectionRenewDeadline,
		Value:       ClusterMgrHA.ElectionRenewDeadline,
	},
	&cli.IntFlag{
		Name:        "cluster-manager-ha-election-retry-period",
		Usage:       "Leader election retry period (in secs) (default: 20)",
		Destination: &cliConfig.ClusterMgrHA.ElectionRetryPeriod,
		Value:       ClusterMgrHA.ElectionRetryPeriod,
	},
}

// HybridOverlayFlags capture hybrid overlay feature options
var HybridOverlayFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:        "enable-hybrid-overlay",
		Usage:       "Enables hybrid overlay functionality",
		Destination: &cliConfig.HybridOverlay.Enabled,
	},
	&cli.StringFlag{
		Name:  "hybrid-overlay-cluster-subnets",
		Value: HybridOverlay.RawClusterSubnets,
		Usage: "A comma separated set of IP subnets and the associated" +
			"hostsubnetlengths (eg, \"10.128.0.0/14/23,10.0.0.0/14/23\"). " +
			"to use with the extended hybrid network. Each entry is given " +
			"in the form IP address/subnet mask/hostsubnetlength, " +
			"the hostsubnetlength is optional and if unspecified defaults to 24. The " +
			"hostsubnetlength defines how many IP addresses are dedicated to each node.",
		Destination: &cliConfig.HybridOverlay.RawClusterSubnets,
	},
	&cli.UintFlag{
		Name:        "hybrid-overlay-vxlan-port",
		Value:       HybridOverlay.VXLANPort,
		Usage:       "The UDP port used by the VXLAN protocol for hybrid networks.",
		Destination: &cliConfig.HybridOverlay.VXLANPort,
	},
}

// OvnKubeNodeFlags captures ovnkube-node specific configurations
var OvnKubeNodeFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "ovnkube-node-mode",
		Usage:       "ovnkube-node operating mode full(default), dpu, dpu-host",
		Value:       OvnKubeNode.Mode,
		Destination: &cliConfig.OvnKubeNode.Mode,
	},
	&cli.StringFlag{
		Name: "ovnkube-node-mgmt-port-netdev",
		Usage: "When provided, use this netdev as management port. It will be renamed to ovn-k8s-mp0 " +
			"and used to allow host network services and pods to access k8s pod and service networks. ",
		Value:       OvnKubeNode.MgmtPortNetdev,
		Destination: &cliConfig.OvnKubeNode.MgmtPortNetdev,
	},
	&cli.StringFlag{
		Name: "ovnkube-node-mgmt-port-dp-resource-name",
		Usage: "When provided, use this device plugin resource name to find the allocated resource as management port. " +
			"The interface chosen from this resource will be renamed to ovn-k8s-mp0 " +
			"and used to allow host network services and pods to access k8s pod and service networks. ",
		Value:       OvnKubeNode.MgmtPortDPResourceName,
		Destination: &cliConfig.OvnKubeNode.MgmtPortDPResourceName,
	},
	&cli.BoolFlag{
		Name:        "disable-ovn-iface-id-ver",
		Usage:       "Deprecated; iface-id-ver is always enabled",
		Destination: &disableOVNIfaceIDVer,
	},
}

// ClusterManagerFlags captures ovnkube-cluster-manager specific configurations
var ClusterManagerFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "cluster-manager-v4-transit-switch-subnet",
		Usage:       "The v4 transit switch subnet used for assigning transit switch IPv4 addresses for interconnect",
		Destination: &cliConfig.ClusterManager.V4TransitSwitchSubnet,
		Value:       ClusterManager.V4TransitSwitchSubnet,
	},
	&cli.StringFlag{
		Name:        "cluster-manager-v6-transit-switch-subnet",
		Usage:       "The v6 transit switch subnet used for assigning transit switch IPv6 addresses for interconnect",
		Destination: &cliConfig.ClusterManager.V6TransitSwitchSubnet,
		Value:       ClusterManager.V6TransitSwitchSubnet,
	},
}

// Flags are general command-line flags. Apps should add these flags to their
// own urfave/cli flags and call InitConfig() early in the application.
var Flags []cli.Flag

// GetFlags returns an array of all command-line flags necessary to configure
// ovn-kubernetes
func GetFlags(customFlags []cli.Flag) []cli.Flag {
	flags := CommonFlags
	flags = append(flags, CNIFlags...)
	flags = append(flags, OVNK8sFeatureFlags...)
	flags = append(flags, K8sFlags...)
	flags = append(flags, MetricsFlags...)
	flags = append(flags, OvnNBFlags...)
	flags = append(flags, OvnSBFlags...)
	flags = append(flags, OVNGatewayFlags...)
	flags = append(flags, MasterHAFlags...)
	flags = append(flags, ClusterMgrHAFlags...)
	flags = append(flags, HybridOverlayFlags...)
	flags = append(flags, MonitoringFlags...)
	flags = append(flags, IPFIXFlags...)
	flags = append(flags, OvnKubeNodeFlags...)
	flags = append(flags, ClusterManagerFlags...)
	flags = append(flags, customFlags...)
	return flags
}

// Defaults are a set of flags to indicate which options should be read from
// ovs-vsctl and used as default values if option is not found via the config
// file or command-line
type Defaults struct {
	OvnNorthAddress bool
	K8sAPIServer    bool
	K8sToken        bool
	K8sTokenFile    bool
	K8sCert         bool
}

const (
	ovsVsctlCommand = "ovs-vsctl"
)

// Can't use pkg/ovs or pkg/util here because those package import this one
func rawExec(exec kexec.Interface, cmd string, args ...string) (string, error) {
	cmdPath, err := exec.LookPath(cmd)
	if err != nil {
		return "", err
	}

	klog.V(5).Infof("Exec: %s %s", cmdPath, strings.Join(args, " "))
	out, err := exec.Command(cmdPath, args...).CombinedOutput()
	if err != nil {
		klog.V(5).Infof("Exec: %s %s => %v", cmdPath, strings.Join(args, " "), err)
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Can't use pkg/ovs or pkg/util here because those package import this one
func runOVSVsctl(exec kexec.Interface, args ...string) (string, error) {
	newArgs := append([]string{"--timeout=15"}, args...)
	out, err := rawExec(exec, ovsVsctlCommand, newArgs...)
	if err != nil {
		return "", err
	}
	return strings.Trim(strings.TrimSpace(out), "\""), nil
}

func getOVSExternalID(exec kexec.Interface, name string) string {
	out, err := runOVSVsctl(exec,
		"--if-exists",
		"get",
		"Open_vSwitch",
		".",
		"external_ids:"+name)
	if err != nil {
		klog.V(5).Infof("Failed to get OVS external_id %s: %v\n\t%s", name, err, out)
		return ""
	}
	return out
}

func setOVSExternalID(exec kexec.Interface, key, value string) error {
	out, err := runOVSVsctl(exec,
		"set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:%s=%s", key, value))
	if err != nil {
		return fmt.Errorf("error setting OVS external ID '%s=%s': %v\n  %q", key, value, err, out)
	}
	return nil
}

func buildKubernetesConfig(exec kexec.Interface, cli, file *config, saPath string, defaults *Defaults) error {
	// token adn ca.crt may be from files mounted in container.
	saConfig := savedKubernetes
	if data, err := os.ReadFile(filepath.Join(saPath, kubeServiceAccountFileToken)); err == nil {
		saConfig.Token = string(data)
		saConfig.TokenFile = filepath.Join(saPath, kubeServiceAccountFileToken)
	}
	if _, err2 := os.Stat(filepath.Join(saPath, kubeServiceAccountFileCACert)); err2 == nil {
		saConfig.CACert = filepath.Join(saPath, kubeServiceAccountFileCACert)
	}

	if err := overrideFields(&Kubernetes, &saConfig, &savedKubernetes); err != nil {
		return err
	}

	// values for token, cacert, kubeconfig, api-server may be found in several places.
	// Priority order (highest first): OVS config, command line options, config file,
	// environment variables, service account files

	envConfig := savedKubernetes
	envVarsMap := map[string]string{
		"Kubeconfig":           "KUBECONFIG",
		"BootstrapKubeconfig":  "BOOTSTRAP_KUBECONFIG",
		"CertDir":              "CERT_DIR",
		"CACert":               "K8S_CACERT",
		"APIServer":            "K8S_APISERVER",
		"Token":                "K8S_TOKEN",
		"TokenFile":            "K8S_TOKEN_FILE",
		"HostNetworkNamespace": "OVN_HOST_NETWORK_NAMESPACE",
	}
	for k, v := range envVarsMap {
		if x, exists := os.LookupEnv(v); exists && len(x) > 0 {
			reflect.ValueOf(&envConfig).Elem().FieldByName(k).SetString(x)
		}
	}

	if err := overrideFields(&Kubernetes, &envConfig, &savedKubernetes); err != nil {
		return err
	}

	// Copy config file values over default values
	if err := overrideFields(&Kubernetes, &file.Kubernetes, &savedKubernetes); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&Kubernetes, &cli.Kubernetes, &savedKubernetes); err != nil {
		return err
	}

	// Grab default values from OVS external IDs
	if defaults.K8sAPIServer {
		Kubernetes.APIServer = getOVSExternalID(exec, "k8s-api-server")
	}
	if defaults.K8sToken {
		Kubernetes.Token = getOVSExternalID(exec, "k8s-api-token")
	}
	if defaults.K8sTokenFile {
		Kubernetes.TokenFile = getOVSExternalID(exec, "k8s-api-token-file")
	}

	if defaults.K8sCert {
		Kubernetes.CACert = getOVSExternalID(exec, "k8s-ca-certificate")
	}

	if Kubernetes.Kubeconfig != "" && !pathExists(Kubernetes.Kubeconfig) {
		return fmt.Errorf("kubernetes kubeconfig file %q not found", Kubernetes.Kubeconfig)
	}

	if Kubernetes.CACert != "" {
		bytes, err := os.ReadFile(Kubernetes.CACert)
		if err != nil {
			return err
		}
		Kubernetes.CAData = bytes
	}

	url, err := url.Parse(Kubernetes.APIServer)
	if err != nil {
		return fmt.Errorf("kubernetes API server address %q invalid: %v", Kubernetes.APIServer, err)
	} else if url.Scheme != "https" && url.Scheme != "http" {
		return fmt.Errorf("kubernetes API server URL scheme %q invalid", url.Scheme)
	}

	// Legacy --service-cluster-ip-range or --k8s-service-cidr options override config file or --k8s-service-cidrs.
	if serviceClusterIPRange != "" {
		Kubernetes.RawServiceCIDRs = serviceClusterIPRange
	} else if Kubernetes.CompatServiceCIDR != "" {
		Kubernetes.RawServiceCIDRs = Kubernetes.CompatServiceCIDR
	}
	if Kubernetes.RawServiceCIDRs == "" {
		return fmt.Errorf("kubernetes service-cidrs is required")
	}

	return nil
}

// completeKubernetesConfig completes the Kubernetes config by parsing raw values
// into their final form.
func completeKubernetesConfig(allSubnets *configSubnets) error {
	Kubernetes.ServiceCIDRs = []*net.IPNet{}
	for _, cidrString := range strings.Split(Kubernetes.RawServiceCIDRs, ",") {
		_, serviceCIDR, err := net.ParseCIDR(cidrString)
		if err != nil {
			return fmt.Errorf("kubernetes service network CIDR %q invalid: %v", cidrString, err)
		}
		Kubernetes.ServiceCIDRs = append(Kubernetes.ServiceCIDRs, serviceCIDR)
		allSubnets.append(configSubnetService, serviceCIDR)
	}
	if len(Kubernetes.ServiceCIDRs) > 2 {
		return fmt.Errorf("kubernetes service-cidrs must contain either a single CIDR or else an IPv4/IPv6 pair")
	} else if len(Kubernetes.ServiceCIDRs) == 2 && utilnet.IsIPv6CIDR(Kubernetes.ServiceCIDRs[0]) == utilnet.IsIPv6CIDR(Kubernetes.ServiceCIDRs[1]) {
		return fmt.Errorf("kubernetes service-cidrs must contain either a single CIDR or else an IPv4/IPv6 pair")
	}

	if Kubernetes.RawNoHostSubnetNodes != "" {
		nodeSelector, err := metav1.ParseToLabelSelector(Kubernetes.RawNoHostSubnetNodes)
		if err != nil {
			return fmt.Errorf("labelSelector \"%s\" is invalid: %v", Kubernetes.RawNoHostSubnetNodes, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(nodeSelector)
		if err != nil {
			return fmt.Errorf("failed to convert %v into a labels.Selector: %v", nodeSelector, err)
		}
		Kubernetes.NoHostSubnetNodes = selector
	}

	return nil
}

func buildMetricsConfig(cli, file *config) error {
	// Copy KubernetesConfig backwards-compat values over default values
	if Kubernetes.CompatMetricsBindAddress != "" {
		Metrics.BindAddress = Kubernetes.CompatMetricsBindAddress
	}
	if Kubernetes.CompatOVNMetricsBindAddress != "" {
		Metrics.OVNMetricsBindAddress = Kubernetes.CompatOVNMetricsBindAddress
	}
	Metrics.EnablePprof = Kubernetes.CompatMetricsEnablePprof

	// Copy config file values over Kubernetes and default values
	if err := overrideFields(&Metrics, &file.Metrics, &savedMetrics); err != nil {
		return err
	}

	// And CLI overrides over config file, Kubernetes, and default values
	if err := overrideFields(&Metrics, &cli.Metrics, &savedMetrics); err != nil {
		return err
	}

	return nil
}

func buildGatewayConfig(ctx *cli.Context, cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&Gateway, &file.Gateway, &savedGateway); err != nil {
		return err
	}

	cli.Gateway.Mode = GatewayMode(ctx.String("gateway-mode"))
	if cli.Gateway.Mode == GatewayModeDisabled {
		// Handle legacy CLI options
		if ctx.Bool("init-gateways") {
			cli.Gateway.Mode = GatewayModeShared
			if ctx.Bool("gateway-local") {
				cli.Gateway.Mode = GatewayModeLocal
			}
		}
	}
	// And CLI overrides over config file and default values
	if err := overrideFields(&Gateway, &cli.Gateway, &savedGateway); err != nil {
		return err
	}

	if Gateway.Mode != GatewayModeDisabled {
		validModes := []string{string(GatewayModeShared), string(GatewayModeLocal)}
		var found bool
		for _, mode := range validModes {
			if string(Gateway.Mode) == mode {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("invalid gateway mode %q: expect one of %s", string(Gateway.Mode), strings.Join(validModes, ","))
		}
	}

	// Options are only valid if Mode is not disabled
	if Gateway.Mode == GatewayModeDisabled {
		if Gateway.Interface != "" {
			return fmt.Errorf("gateway interface option %q not allowed when gateway is disabled", Gateway.Interface)
		}
		if Gateway.NextHop != "" {
			return fmt.Errorf("gateway next-hop option %q not allowed when gateway is disabled", Gateway.NextHop)
		}
	}

	if Gateway.Mode != GatewayModeShared && Gateway.VLANID != 0 {
		return fmt.Errorf("gateway VLAN ID option: %d is supported only in shared gateway mode", Gateway.VLANID)
	}

	return nil
}

func completeGatewayConfig(allSubnets *configSubnets, masqueradeIPs *MasqueradeIPsConfig) error {
	// Validate v4 and v6 join subnets
	v4IP, v4JoinCIDR, err := net.ParseCIDR(Gateway.V4JoinSubnet)
	if err != nil || utilnet.IsIPv6(v4IP) {
		return fmt.Errorf("invalid gateway v4 join subnet specified, subnet: %s: error: %v", Gateway.V4JoinSubnet, err)
	}

	v6IP, v6JoinCIDR, err := net.ParseCIDR(Gateway.V6JoinSubnet)
	if err != nil || !utilnet.IsIPv6(v6IP) {
		return fmt.Errorf("invalid gateway v6 join subnet specified, subnet: %s: error: %v", Gateway.V6JoinSubnet, err)
	}
	allSubnets.append(configSubnetJoin, v4JoinCIDR)
	allSubnets.append(configSubnetJoin, v6JoinCIDR)

	//validate v4 and v6 masquerade subnets
	v4MasqueradeIP, v4MasqueradeCIDR, err := net.ParseCIDR(Gateway.V4MasqueradeSubnet)
	if err != nil || utilnet.IsIPv6(v4MasqueradeCIDR.IP) {
		return fmt.Errorf("invalid gateway v4 masquerade subnet specified, subnet: %s: error: %v", Gateway.V4MasqueradeSubnet, err)
	}
	if err = allocateV4MasqueradeIPs(v4MasqueradeIP, masqueradeIPs); err != nil {
		return fmt.Errorf("unable to allocate V4MasqueradeIPs: %s", err)
	}

	v6MasqueradeIP, v6MasqueradeCIDR, err := net.ParseCIDR(Gateway.V6MasqueradeSubnet)
	if err != nil || !utilnet.IsIPv6(v6MasqueradeCIDR.IP) {
		return fmt.Errorf("invalid gateway v6 masquerade subnet specified, subnet: %s: error: %v", Gateway.V6MasqueradeSubnet, err)
	}
	if err = allocateV6MasqueradeIPs(v6MasqueradeIP, masqueradeIPs); err != nil {
		return fmt.Errorf("unable to allocate V6MasqueradeIPs: %s", err)
	}

	allSubnets.append(configSubnetMasquerade, v4MasqueradeCIDR)
	allSubnets.append(configSubnetMasquerade, v6MasqueradeCIDR)

	return nil
}

func buildOVNKubernetesFeatureConfig(ctx *cli.Context, cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&OVNKubernetesFeature, &file.OVNKubernetesFeature, &savedOVNKubernetesFeature); err != nil {
		return err
	}
	// And CLI overrides over config file and default values
	if err := overrideFields(&OVNKubernetesFeature, &cli.OVNKubernetesFeature, &savedOVNKubernetesFeature); err != nil {
		return err
	}
	return nil
}

func buildMasterHAConfig(ctx *cli.Context, cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&MasterHA, &file.MasterHA, &savedMasterHA); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&MasterHA, &cli.MasterHA, &savedMasterHA); err != nil {
		return err
	}

	if MasterHA.ElectionLeaseDuration <= MasterHA.ElectionRenewDeadline {
		return fmt.Errorf("invalid HA election lease duration '%d'. "+
			"It should be greater than HA election renew deadline '%d'",
			MasterHA.ElectionLeaseDuration, MasterHA.ElectionRenewDeadline)
	}

	if MasterHA.ElectionRenewDeadline <= MasterHA.ElectionRetryPeriod {
		return fmt.Errorf("invalid HA election renew deadline duration '%d'. "+
			"It should be greater than HA election retry period '%d'",
			MasterHA.ElectionRenewDeadline, MasterHA.ElectionRetryPeriod)
	}
	return nil
}

func buildClusterMgrHAConfig(ctx *cli.Context, cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&ClusterMgrHA, &file.ClusterMgrHA, &savedClusterMgrHA); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&ClusterMgrHA, &cli.ClusterMgrHA, &savedClusterMgrHA); err != nil {
		return err
	}

	if ClusterMgrHA.ElectionLeaseDuration <= ClusterMgrHA.ElectionRenewDeadline {
		return fmt.Errorf("invalid HA election lease duration '%d'. "+
			"It should be greater than HA election renew deadline '%d'",
			ClusterMgrHA.ElectionLeaseDuration, ClusterMgrHA.ElectionRenewDeadline)
	}

	if ClusterMgrHA.ElectionRenewDeadline <= ClusterMgrHA.ElectionRetryPeriod {
		return fmt.Errorf("invalid HA election renew deadline duration '%d'. "+
			"It should be greater than HA election retry period '%d'",
			ClusterMgrHA.ElectionRenewDeadline, ClusterMgrHA.ElectionRetryPeriod)
	}
	return nil
}

func buildMonitoringConfig(ctx *cli.Context, cli, file *config) error {
	var err error
	if err = overrideFields(&Monitoring, &file.Monitoring, &savedMonitoring); err != nil {
		return err
	}
	if err = overrideFields(&Monitoring, &cli.Monitoring, &savedMonitoring); err != nil {
		return err
	}
	return nil
}

// completeMonitoringConfig completes the Monitoring config by parsing raw values
// into their final form.
func completeMonitoringConfig() error {
	var err error
	if Monitoring.RawNetFlowTargets != "" {
		Monitoring.NetFlowTargets, err = ParseFlowCollectors(Monitoring.RawNetFlowTargets)
		if err != nil {
			return fmt.Errorf("netflow targets invalid: %v", err)
		}
	}
	if Monitoring.RawSFlowTargets != "" {
		Monitoring.SFlowTargets, err = ParseFlowCollectors(Monitoring.RawSFlowTargets)
		if err != nil {
			return fmt.Errorf("sflow targets invalid: %v", err)
		}
	}
	if Monitoring.RawIPFIXTargets != "" {
		Monitoring.IPFIXTargets, err = ParseFlowCollectors(Monitoring.RawIPFIXTargets)
		if err != nil {
			return fmt.Errorf("ipfix targets invalid: %v", err)
		}
	}
	return nil
}

func buildIPFIXConfig(cli, file *config) error {
	if err := overrideFields(&IPFIX, &file.IPFIX, &savedIPFIX); err != nil {
		return err
	}
	return overrideFields(&IPFIX, &cli.IPFIX, &savedIPFIX)
}

func buildHybridOverlayConfig(ctx *cli.Context, cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&HybridOverlay, &file.HybridOverlay, &savedHybridOverlay); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&HybridOverlay, &cli.HybridOverlay, &savedHybridOverlay); err != nil {
		return err
	}

	if HybridOverlay.Enabled && HybridOverlay.VXLANPort > 65535 {
		return fmt.Errorf("hybrid overlay vxlan port is invalid. The port cannot be larger than 65535")
	}

	return nil
}

// completeHybridOverlayConfig completes the HybridOverlay config by parsing raw values
// into their final form.
func completeHybridOverlayConfig(allSubnets *configSubnets) error {
	if !HybridOverlay.Enabled || len(HybridOverlay.RawClusterSubnets) == 0 {
		return nil
	}

	var err error
	HybridOverlay.ClusterSubnets, err = ParseClusterSubnetEntries(HybridOverlay.RawClusterSubnets)
	if err != nil {
		return fmt.Errorf("hybrid overlay cluster subnet invalid: %v", err)
	}
	for _, subnet := range HybridOverlay.ClusterSubnets {
		allSubnets.append(configSubnetHybrid, subnet.CIDR)
	}

	return nil
}

func buildClusterManagerConfig(ctx *cli.Context, cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&ClusterManager, &file.ClusterManager, &savedClusterManager); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&ClusterManager, &cli.ClusterManager, &savedClusterManager); err != nil {
		return err
	}

	return nil
}

// completeClusterManagerConfig completes the ClusterManager config by parsing raw values
// into their final form.
func completeClusterManagerConfig() error {
	// Validate v4 and v6 transit switch subnets
	v4IP, _, err := net.ParseCIDR(ClusterManager.V4TransitSwitchSubnet)
	if err != nil || utilnet.IsIPv6(v4IP) {
		return fmt.Errorf("invalid transit switch v4 subnet specified, subnet: %s: error: %v", ClusterManager.V4TransitSwitchSubnet, err)
	}

	v6IP, _, err := net.ParseCIDR(ClusterManager.V6TransitSwitchSubnet)
	if err != nil || !utilnet.IsIPv6(v6IP) {
		return fmt.Errorf("invalid transit switch v6 subnet specified, subnet: %s: error: %v", ClusterManager.V6TransitSwitchSubnet, err)
	}

	return nil
}

func buildDefaultConfig(cli, file *config) error {
	if err := overrideFields(&Default, &file.Default, &savedDefault); err != nil {
		return err
	}

	if err := overrideFields(&Default, &cli.Default, &savedDefault); err != nil {
		return err
	}

	// Legacy cluster-subnet CLI option overrides config file or --cluster-subnets
	if clusterSubnet != "" {
		Default.RawClusterSubnets = clusterSubnet
	}
	if Default.RawClusterSubnets == "" {
		return fmt.Errorf("cluster subnet is required")
	}

	if Default.Zone == "" {
		Default.Zone = types.OvnDefaultZone
	}
	return nil
}

// completeDefaultConfig completes the Default config by parsing raw values
// into their final form.
func completeDefaultConfig(allSubnets *configSubnets) error {
	var err error
	Default.ClusterSubnets, err = ParseClusterSubnetEntries(Default.RawClusterSubnets)
	if err != nil {
		return fmt.Errorf("cluster subnet invalid: %v", err)
	}
	for _, subnet := range Default.ClusterSubnets {
		allSubnets.append(configSubnetCluster, subnet.CIDR)
	}

	Default.HostMasqConntrackZone = Default.ConntrackZone + 1
	Default.OVNMasqConntrackZone = Default.ConntrackZone + 2
	Default.HostNodePortConntrackZone = Default.ConntrackZone + 3
	Default.ReassemblyConntrackZone = Default.ConntrackZone + 4
	return nil
}

// getConfigFilePath returns config file path and 'true' if the config file is
// the fallback path (eg not given by the user), 'false' if given explicitly
// by the user
func getConfigFilePath(ctx *cli.Context) (string, bool) {
	configFile := ctx.String("config-file")
	if configFile != "" {
		return configFile, false
	}
	return "/etc/openvswitch/ovn_k8s.conf", true
}

// InitConfig reads the config file and common command-line options and
// constructs the global config object from them. It returns the config file
// path (if explicitly specified) or an error
func InitConfig(ctx *cli.Context, exec kexec.Interface, defaults *Defaults) (string, error) {
	return initConfigWithPath(ctx, exec, kubeServiceAccountPath, defaults)
}

// InitConfigSa reads the config file and common command-line options and
// constructs the global config object from them. It passes the service account directory.
// It returns the config file path (if explicitly specified) or an error
func InitConfigSa(ctx *cli.Context, exec kexec.Interface, saPath string, defaults *Defaults) (string, error) {
	return initConfigWithPath(ctx, exec, saPath, defaults)
}

// stripTokenFromK8sConfig removes k8s SA token & CAData values
// from the KubernetesConfig struct used for logging.
func stripTokenFromK8sConfig() KubernetesConfig {
	k8sConf := Kubernetes
	// Token and CAData are sensitive fields so stripping
	// them while logging.
	k8sConf.Token = ""
	k8sConf.CAData = []byte{}
	return k8sConf
}

// initConfigWithPath reads the given config file (or if empty, reads the config file
// specified by command-line arguments, or empty, the default config file) and
// common command-line options and constructs the global config object from
// them. It returns the config file path (if explicitly specified) or an error
func initConfigWithPath(ctx *cli.Context, exec kexec.Interface, saPath string, defaults *Defaults) (string, error) {
	var retConfigFile string
	var configFile string
	var configFileIsDefault bool
	var err error
	// initialize cfg with default values, allow file read to override
	cfg := config{
		Default:              savedDefault,
		Logging:              savedLogging,
		IPFIX:                savedIPFIX,
		CNI:                  savedCNI,
		OVNKubernetesFeature: savedOVNKubernetesFeature,
		Kubernetes:           savedKubernetes,
		OvnNorth:             savedOvnNorth,
		OvnSouth:             savedOvnSouth,
		Gateway:              savedGateway,
		MasterHA:             savedMasterHA,
		ClusterMgrHA:         savedClusterMgrHA,
		HybridOverlay:        savedHybridOverlay,
		OvnKubeNode:          savedOvnKubeNode,
		ClusterManager:       savedClusterManager,
	}

	configFile, configFileIsDefault = getConfigFilePath(ctx)

	if !configFileIsDefault {
		// Only return explicitly specified config file
		retConfigFile = configFile
	}

	f, err := os.Open(configFile)
	// Failure to find a default config file is not a hard error
	if err != nil && !configFileIsDefault {
		return "", fmt.Errorf("failed to open config file %s: %v", configFile, err)
	}
	if f != nil {
		defer f.Close()

		// Parse ovn-k8s config file.
		if err = gcfg.ReadInto(&cfg, f); err != nil {
			if gcfg.FatalOnly(err) != nil {
				return "", fmt.Errorf("failed to parse config file %s: %v", f.Name(), err)
			}
			// error is only a warning -> log it but continue
			klog.Warningf("Warning on parsing config file: %s", err)
		}
		klog.Infof("Parsed config file %s", f.Name())
		klog.Infof("Parsed config: %+v", cfg)
	}

	if defaults == nil {
		defaults = &Defaults{}
	}

	// Build config that needs no special processing
	if err = overrideFields(&CNI, &cfg.CNI, &savedCNI); err != nil {
		return "", err
	}
	if err = overrideFields(&CNI, &cliConfig.CNI, &savedCNI); err != nil {
		return "", err
	}

	// Logging setup
	if err = overrideFields(&Logging, &cfg.Logging, &savedLogging); err != nil {
		return "", err
	}
	if err = overrideFields(&Logging, &cliConfig.Logging, &savedLogging); err != nil {
		return "", err
	}

	var level klog.Level
	if err := level.Set(strconv.Itoa(Logging.Level)); err != nil {
		return "", fmt.Errorf("failed to set klog log level %v", err)
	}
	if Logging.File != "" {
		klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
		klog.InitFlags(klogFlags)
		if err := klogFlags.Set("logtostderr", "false"); err != nil {
			klog.Errorf("Error setting klog logtostderr: %v", err)
		}
		if err := klogFlags.Set("alsologtostderr", "true"); err != nil {
			klog.Errorf("Error setting klog alsologtostderr: %v", err)
		}
		klog.SetOutput(&lumberjack.Logger{
			Filename:   Logging.File,
			MaxSize:    Logging.LogFileMaxSize, // megabytes
			MaxBackups: Logging.LogFileMaxBackups,
			MaxAge:     Logging.LogFileMaxAge, // days
			Compress:   true,
		})
	}

	if err = buildDefaultConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildKubernetesConfig(exec, &cliConfig, &cfg, saPath, defaults); err != nil {
		return "", err
	}

	// Metrics must be built after Kubernetes to ensure metrics options override
	// legacy Kubernetes metrics options
	if err = buildMetricsConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildOVNKubernetesFeatureConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildGatewayConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildMasterHAConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildClusterMgrHAConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildMonitoringConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildIPFIXConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildHybridOverlayConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildOvnKubeNodeConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildClusterManagerConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	tmpAuth, err := buildOvnAuth(exec, true, &cliConfig.OvnNorth, &cfg.OvnNorth, defaults.OvnNorthAddress)
	if err != nil {
		return "", err
	}
	OvnNorth = *tmpAuth

	tmpAuth, err = buildOvnAuth(exec, false, &cliConfig.OvnSouth, &cfg.OvnSouth, false)
	if err != nil {
		return "", err
	}
	OvnSouth = *tmpAuth

	if err := completeConfig(); err != nil {
		return "", err
	}

	klog.V(5).Infof("Default config: %+v", Default)
	klog.V(5).Infof("Logging config: %+v", Logging)
	klog.V(5).Infof("Monitoring config: %+v", Monitoring)
	klog.V(5).Infof("IPFIX config: %+v", IPFIX)
	klog.V(5).Infof("CNI config: %+v", CNI)
	klog.V(5).Infof("Kubernetes config: %+v", stripTokenFromK8sConfig())
	klog.V(5).Infof("Gateway config: %+v", Gateway)
	klog.V(5).Infof("OVN North config: %+v", OvnNorth)
	klog.V(5).Infof("OVN South config: %+v", OvnSouth)
	klog.V(5).Infof("Hybrid Overlay config: %+v", HybridOverlay)
	klog.V(5).Infof("Ovnkube Node config: %+v", OvnKubeNode)
	klog.V(5).Infof("Ovnkube Cluster Manager config: %+v", ClusterManager)

	return retConfigFile, nil
}

func completeConfig() error {
	allSubnets := newConfigSubnets()

	if err := completeKubernetesConfig(allSubnets); err != nil {
		return err
	}
	if err := completeDefaultConfig(allSubnets); err != nil {
		return err
	}

	if err := completeGatewayConfig(allSubnets, &Gateway.MasqueradeIPs); err != nil {
		return err
	}
	if err := completeMonitoringConfig(); err != nil {
		return err
	}
	if err := completeHybridOverlayConfig(allSubnets); err != nil {
		return err
	}

	if err := completeClusterManagerConfig(); err != nil {
		return err
	}

	if err := allSubnets.checkForOverlaps(); err != nil {
		return err
	}

	var err error
	IPv4Mode, IPv6Mode, err = allSubnets.checkIPFamilies()
	if err != nil {
		return err
	}

	return nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	if err != nil && os.IsNotExist(err) {
		return false
	}
	return true
}

// parseAddress parses an OVN database address, which can be of form
// "ssl:1.2.3.4:6641,ssl:1.2.3.5:6641" (OVS/OVN format) or
// "ssl://1.2.3.4:6641,ssl://1.2.3.5:6641" (legacy ovnkube format)
// or "ssl:[fd01::1]:6641,ssl:[fd01::2]:6641
// and returns the validated address(es) and the scheme
func parseAddress(urlString string) (string, OvnDBScheme, error) {
	var parsedAddress, scheme string
	var parsedScheme OvnDBScheme

	urlString = strings.Replace(urlString, "//", "", -1)
	for _, ovnAddress := range strings.Split(urlString, ",") {
		splits := strings.SplitN(ovnAddress, ":", 2)
		if len(splits) != 2 {
			return "", "", fmt.Errorf("failed to parse OVN address %s", urlString)
		}

		if scheme == "" {
			scheme = splits[0]
		} else if scheme != splits[0] {
			return "", "", fmt.Errorf("invalid protocols in OVN address %s",
				urlString)
		}

		if scheme == "unix" {
			if parsedAddress != "" {
				parsedAddress += ","
			}
			parsedAddress += ovnAddress
		} else {
			host, port, err := net.SplitHostPort(splits[1])
			if err != nil {
				return "", "", fmt.Errorf("failed to parse OVN DB host/port %q: %v",
					splits[1], err)
			}

			if parsedAddress != "" {
				parsedAddress += ","
			}
			parsedAddress += fmt.Sprintf("%s:%s", scheme, net.JoinHostPort(host, port))
		}
	}

	switch {
	case scheme == "ssl":
		parsedScheme = OvnDBSchemeSSL
	case scheme == "tcp":
		parsedScheme = OvnDBSchemeTCP
	case scheme == "unix":
		parsedScheme = OvnDBSchemeUnix
	default:
		return "", "", fmt.Errorf("unknown OVN DB scheme %q", scheme)
	}
	return parsedAddress, parsedScheme, nil
}

// buildOvnAuth returns an OvnAuthConfig object describing the connection to an
// OVN database, given a connection description string and authentication
// details
func buildOvnAuth(exec kexec.Interface, northbound bool, cliAuth, confAuth *OvnAuthConfig, readAddress bool) (*OvnAuthConfig, error) {
	auth := &OvnAuthConfig{
		northbound: northbound,
		exec:       exec,
	}

	var direction string
	var defaultAuth *OvnAuthConfig
	if northbound {
		direction = "nb"
		defaultAuth = &savedOvnNorth
	} else {
		direction = "sb"
		defaultAuth = &savedOvnSouth
	}

	// Determine final address so we know how to set cert/key defaults
	address := cliAuth.Address
	if address == "" {
		address = confAuth.Address
	}
	if address == "" && readAddress {
		address = getOVSExternalID(exec, "ovn-"+direction)
	}
	if strings.HasPrefix(address, "ssl") {
		// Set up default SSL cert/key paths
		auth.CACert = "/etc/openvswitch/ovn" + direction + "-ca.cert"
		auth.PrivKey = "/etc/openvswitch/ovn" + direction + "-privkey.pem"
		auth.Cert = "/etc/openvswitch/ovn" + direction + "-cert.pem"
	}

	// Build the final auth config with overrides from CLI and config file
	if err := overrideFields(auth, confAuth, defaultAuth); err != nil {
		return nil, err
	}
	if err := overrideFields(auth, cliAuth, defaultAuth); err != nil {
		return nil, err
	}

	if address == "" {
		if auth.PrivKey != "" || auth.Cert != "" || auth.CACert != "" {
			return nil, fmt.Errorf("certificate or key given; perhaps you mean to use the 'ssl' scheme?")
		}
		auth.Scheme = OvnDBSchemeUnix
		auth.Address = fmt.Sprintf("unix:/var/run/ovn/ovn%s_db.sock", direction)
		return auth, nil
	}

	var err error
	auth.Address, auth.Scheme, err = parseAddress(address)
	if err != nil {
		return nil, err
	}

	switch {
	case auth.Scheme == OvnDBSchemeSSL:
		if auth.PrivKey == "" || auth.Cert == "" || auth.CACert == "" || auth.CertCommonName == "" {
			return nil, fmt.Errorf("must specify private key, certificate, CA certificate, and common name used in the certificate for 'ssl' scheme")
		}
	case auth.Scheme == OvnDBSchemeTCP:
		if auth.PrivKey != "" || auth.Cert != "" || auth.CACert != "" {
			return nil, fmt.Errorf("certificate or key given; perhaps you mean to use the 'ssl' scheme?")
		}
	case auth.Scheme == OvnDBSchemeUnix:
		if auth.PrivKey != "" || auth.Cert != "" || auth.CACert != "" {
			return nil, fmt.Errorf("certificate or key given; perhaps you mean to use the 'ssl' scheme?")
		}
	}

	return auth, nil
}

func (a *OvnAuthConfig) ensureCACert() error {
	if pathExists(a.CACert) {
		// CA file exists, nothing to do
		return nil
	}

	// Client can bootstrap the CA from the OVN API.  Use nbctl for both
	// SB and NB since ovn-sbctl only supports --bootstrap-ca-cert from
	// 2.9.90+.
	// FIXME: change back to a.ctlCmd when sbctl supports --bootstrap-ca-cert
	// https://github.com/openvswitch/ovs/pull/226
	args := []string{
		"--db=" + a.GetURL(),
		"--timeout=5",
	}
	if a.Scheme == OvnDBSchemeSSL {
		args = append(args, "--private-key="+a.PrivKey)
		args = append(args, "--certificate="+a.Cert)
		args = append(args, "--bootstrap-ca-cert="+a.CACert)
	}
	args = append(args, "list", "nb_global")
	_, _ = rawExec(a.exec, "ovn-nbctl", args...)
	if _, err := os.Stat(a.CACert); os.IsNotExist(err) {
		klog.Warningf("Bootstrapping %s CA certificate failed", a.CACert)
	}
	return nil
}

// GetURL returns a URL suitable for passing to ovn-northd which describes the
// transport mechanism for connection to the database
func (a *OvnAuthConfig) GetURL() string {
	return a.Address
}

// SetDBAuth sets the authentication configuration and connection method
// for the OVN northbound or southbound database server or client
func (a *OvnAuthConfig) SetDBAuth() error {
	if a.Scheme == OvnDBSchemeSSL {
		// Both server and client SSL schemes require privkey and cert
		if !pathExists(a.PrivKey) {
			return fmt.Errorf("private key file %s not found", a.PrivKey)
		}
		if !pathExists(a.Cert) {
			return fmt.Errorf("certificate file %s not found", a.Cert)
		}

		// Client can bootstrap the CA cert from the DB
		if err := a.ensureCACert(); err != nil {
			return err
		}

		// Tell Southbound DB clients (like ovn-controller)
		// which certificates to use to talk to the DB.
		// Must happen *before* setting the "ovn-remote"
		// external-id.
		if !a.northbound {
			out, err := runOVSVsctl(a.exec, "del-ssl")
			if err != nil {
				return fmt.Errorf("error deleting ovs-vsctl SSL "+
					"configuration: %q (%v)", out, err)
			}

			out, err = runOVSVsctl(a.exec, "set-ssl", a.PrivKey, a.Cert, a.CACert)
			if err != nil {
				return fmt.Errorf("error setting client southbound DB SSL options: %v\n  %q", err, out)
			}
		}
	}

	if !a.northbound {
		// store the Southbound Database address in an external id - "external_ids:ovn-remote"
		if err := setOVSExternalID(a.exec, "ovn-remote", "\""+a.GetURL()+"\""); err != nil {
			return err
		}
	}

	return nil
}

func (a *OvnAuthConfig) updateIP(newIPs []string, port string) {
	newAddresses := make([]string, 0, len(newIPs))
	for _, ipAddress := range newIPs {
		newAddresses = append(newAddresses, fmt.Sprintf("%v:%s", a.Scheme, net.JoinHostPort(ipAddress, port)))
	}
	a.Address = strings.Join(newAddresses, ",")
}

// UpdateOVNNodeAuth updates the host and URL in ClientAuth
// for both OvnNorth and OvnSouth. It updates them with the new masterIP.
func UpdateOVNNodeAuth(masterIP []string, southboundDBPort, northboundDBPort string) {
	klog.V(5).Infof("Update OVN node auth with new master ip: %s", masterIP)
	OvnNorth.updateIP(masterIP, northboundDBPort)
	OvnSouth.updateIP(masterIP, southboundDBPort)
}

// ovnKubeNodeModeSupported validates the provided mode is supported by ovnkube node
func ovnKubeNodeModeSupported(mode string) error {
	found := false
	supportedModes := []string{types.NodeModeFull, types.NodeModeDPU, types.NodeModeDPUHost}
	for _, m := range supportedModes {
		if mode == m {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("unexpected ovnkube-node-mode: %s. supported modes: %v", mode, supportedModes)
	}
	return nil
}

// buildOvnKubeNodeConfig updates OvnKubeNode config from cli and config file
func buildOvnKubeNodeConfig(ctx *cli.Context, cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&OvnKubeNode, &file.OvnKubeNode, &savedOvnKubeNode); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&OvnKubeNode, &cli.OvnKubeNode, &savedOvnKubeNode); err != nil {
		return err
	}

	// validate ovnkube-node-mode
	if err := ovnKubeNodeModeSupported(OvnKubeNode.Mode); err != nil {
		return err
	}

	// ovnkube-node-mode dpu/dpu-host does not support hybrid overlay
	if OvnKubeNode.Mode != types.NodeModeFull && HybridOverlay.Enabled {
		return fmt.Errorf("hybrid overlay is not supported with ovnkube-node mode %s", OvnKubeNode.Mode)
	}

	// Warn the user if both MgmtPortNetdev and MgmtPortDPResourceName are specified since they
	// configure the management port.
	if OvnKubeNode.MgmtPortNetdev != "" && OvnKubeNode.MgmtPortDPResourceName != "" {
		klog.Warningf("ovnkube-node-mgmt-port-netdev (%s) and ovnkube-node-mgmt-port-dp-resource-name (%s) "+
			"both specified. The provided netdev in ovnkube-node-mgmt-port-netdev will be overriden by a netdev "+
			"chosen by the resource provided by ovnkube-node-mgmt-port-dp-resource-name.",
			OvnKubeNode.MgmtPortNetdev, OvnKubeNode.MgmtPortDPResourceName)
	}

	// when DPU is used, management port is always backed by a representor. On the
	// host side, it needs to be provided through --ovnkube-node-mgmt-port-netdev.
	// On the DPU, it is derrived from the annotation exposed on the host side.
	if OvnKubeNode.Mode == types.NodeModeDPU && !(OvnKubeNode.MgmtPortNetdev == "" && OvnKubeNode.MgmtPortDPResourceName == "") {
		return fmt.Errorf("ovnkube-node-mgmt-port-netdev or ovnkube-node-mgmt-port-dp-resource-name must not be provided")
	}
	if OvnKubeNode.Mode == types.NodeModeDPUHost && OvnKubeNode.MgmtPortNetdev == "" && OvnKubeNode.MgmtPortDPResourceName == "" {
		return fmt.Errorf("ovnkube-node-mgmt-port-netdev or ovnkube-node-mgmt-port-dp-resource-name must be provided")
	}
	return nil
}
