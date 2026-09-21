// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	v1 "github.com/openshift/api/config/v1"
	"github.com/urfave/cli/v2"
	gcfg "gopkg.in/gcfg.v1"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/tls"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// getSupportedPlatformTypes returns a list of all supported platform types
func getSupportedPlatformTypes() []string {
	return []string{
		string(v1.AWSPlatformType),          // "AWS"
		string(v1.AzurePlatformType),        // "Azure"
		string(v1.BareMetalPlatformType),    // "BareMetal"
		string(v1.GCPPlatformType),          // "GCP"
		string(v1.LibvirtPlatformType),      // "Libvirt"
		string(v1.OpenStackPlatformType),    // "OpenStack"
		string(v1.NonePlatformType),         // "None"
		string(v1.VSpherePlatformType),      // "VSphere"
		string(v1.OvirtPlatformType),        // "oVirt"
		string(v1.IBMCloudPlatformType),     // "IBMCloud"
		string(v1.KubevirtPlatformType),     // "KubeVirt"
		string(v1.EquinixMetalPlatformType), // "EquinixMetal"
		string(v1.PowerVSPlatformType),      // "PowerVS"
		string(v1.AlibabaCloudPlatformType), // "AlibabaCloud"
		string(v1.NutanixPlatformType),      // "Nutanix"
		string(v1.ExternalPlatformType),     // "External"
	}
}

// DefaultEncapPort number used if not supplied
const DefaultEncapPort = 6081

const DefaultAPIServer = "http://localhost:8443"

// Default IANA-assigned UDP port number for VXLAN
const DefaultVXLANPort = 4789

const DefaultDBTxnTimeout = time.Second * 100

// DefaultEphemeralPortRange is used for unit testing only
const DefaultEphemeralPortRange = "32768-60999"

// MinimumRoutingTableIDStart is the lowest allowed start of OVN-managed Linux route table IDs.
const MinimumRoutingTableIDStart = 1000

// MaximumRoutingTableIDStart is capped so adding any Linux interface index still fits in a uint32 route table ID.
const MaximumRoutingTableIDStart = 1<<31 - 1

// DefaultRoutingTableIDStart is the default start of OVN-managed Linux route table IDs.
const DefaultRoutingTableIDStart = MinimumRoutingTableIDStart

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
	Version = "1.4.0"
	// version of the go runtime used to compile ovn-kubernetes
	GoVersion = runtime.Version()
	// os and architecture used to build ovn-kubernetes
	OSArch = fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)

	// ovn-kubernetes cni config file name
	CNIConfFileName = "10-ovn-kubernetes.conf"

	// Default holds parsed config file parameters and command-line overrides
	Default = DefaultConfig{
		MTU:                          1400,
		ConntrackZone:                64000,
		EncapType:                    "geneve",
		EncapIP:                      "",
		EncapPort:                    DefaultEncapPort,
		InactivityProbe:              100000, // in Milliseconds
		OpenFlowProbe:                0,      // in Milliseconds
		OfctrlWaitBeforeClear:        0,      // in Milliseconds
		MonitorAll:                   true,
		OVSDBTxnTimeout:              DefaultDBTxnTimeout,
		LFlowCacheEnable:             true,
		RawClusterSubnets:            "10.128.0.0/14/23",
		RawUDNAllowedDefaultServices: "default/kubernetes,kube-system/kube-dns",
		Transport:                    "",
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

	// TLS holds TLS-related configuration parameters.
	TLS TLSConfig

	// OVNKubernetesFeature config holds OVN-Kubernetes feature enhancement config file parameters and command-line overrides
	OVNKubernetesFeature = OVNKubernetesFeatureConfig{
		EgressIPReachabiltyTotalTimeout: 1,
		AdvertisedUDNIsolationMode:      AdvertisedUDNIsolationModeStrict,
		UDNDeletionGracePeriod:          120 * time.Second,
	}

	// OvnNorth holds local northbound database paths.
	OvnNorth = OvnDBConfig{
		northbound: true,
		RunDir:     "/var/run/ovn/",
		DbLocation: "/etc/ovn/ovnnb_db.db",
	}

	// OvnSouth holds local southbound database paths.
	OvnSouth = OvnDBConfig{
		RunDir:     "/var/run/ovn/",
		DbLocation: "/etc/ovn/ovnsb_db.db",
	}

	// EgressIPHealthCheckTLS holds TLS settings for the Egress IP gRPC health-check channel.
	EgressIPHealthCheckTLS EgressIPHealthCheckTLSConfig

	// OvsPaths holds OVS location details
	OvsPaths = OvsPathConfig{
		RunDir: "/var/run/openvswitch/",
	}

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
		Mode:                      types.NodeModeFull,
		DPUNodeLeaseRenewInterval: 10,
		DPUNodeLeaseDuration:      40,
		RoutingTableIDStart:       DefaultRoutingTableIDStart,
	}

	ClusterManager = ClusterManagerConfig{
		V4TransitSubnet: "100.88.0.0/16",
		V6TransitSubnet: "fd97::/64",
	}

	// NoOverlay holds no-overlay mode configuration
	NoOverlay = NoOverlayConfig{}

	// ManagedBGP holds managed BGP configuration
	ManagedBGP = ManagedBGPConfig{
		ASNumber: 64512, // Default AS number
	}

	// Layer2UsesTransitRouter selects the current primary Layer2 topology.
	// Both controller managers enable it at startup.
	//
	// DEPRECATED: retained only until the legacy topology branches are removed.
	Layer2UsesTransitRouter bool
)

const (
	kubeServiceAccountPath       string = "/var/run/secrets/kubernetes.io/serviceaccount/"
	kubeServiceAccountFileToken  string = "token"
	kubeServiceAccountFileCACert string = "ca.crt"
)

// No-overlay mode configuration option constants
const (
	// NoOverlayRoutingManaged indicates OVN-Kubernetes manages the routing
	NoOverlayRoutingManaged string = "managed"
	// NoOverlayRoutingUnmanaged indicates users manage the routing themselves
	NoOverlayRoutingUnmanaged string = "unmanaged"

	// ManagedBGPTopologyFullMesh represents a full-mesh BGP topology
	ManagedBGPTopologyFullMesh string = "full-mesh"
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
	// Configured IP address of the encapsulation endpoint.
	EncapIP string `gcfg:"encap-ip"`
	// Effective encap IP. It may be different from EncapIP if EncapIP meant to be
	// the node's primary IP which can be updated when node's primary IP changes.
	EffectiveEncapIP string
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
	// OVSDBTxnTimeout is the timeout for db transaction, may be useful to increase for high-scale clusters.
	// default value is 100 seconds.
	OVSDBTxnTimeout time.Duration `gcfg:"db-txn-timeout"`
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

	// RawUDNAllowedDefaultServices holds the unparsed UDNAllowedDefaultServices. Should only be
	// used inside config module.
	RawUDNAllowedDefaultServices string `gcfg:"udn-allowed-default-services"`

	// UDNAllowedDefaultServices holds a list of namespaced names of
	// default cluster network services accessible from primary user-defined networks
	UDNAllowedDefaultServices []string

	// Transport specifies the transport technology used for the default network.
	// Accepts: "" (empty, uses OVN default overlay) or "no-overlay".
	// Defaults to "" (empty).
	Transport string `gcfg:"transport"`

	// ClusterDefaultNADName is the namespace/name of the default cluster network
	// NAD. When empty, it resolves to OVNConfigNamespace/default.
	ClusterDefaultNADName string `gcfg:"cluster-default-nad"`
	// ClusterDefaultNetworkNAD is the parsed form of ClusterDefaultNADName,
	// populated in completeDefaultConfig.
	ClusterDefaultNetworkNAD *nettypes.NetworkSelectionElement
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
	CACertData              string        `gcfg:"cacert-data"`
	CAData                  []byte
	APIServer               string `gcfg:"apiserver"`
	Token                   string `gcfg:"token"`
	TokenFile               string `gcfg:"tokenFile"`
	CompatServiceCIDR       string `gcfg:"service-cidr"`
	RawServiceCIDRs         string `gcfg:"service-cidrs"`
	ServiceCIDRs            []*net.IPNet
	OVNConfigNamespace      string `gcfg:"ovn-config-namespace"`
	OVNEmptyLbEvents        bool   `gcfg:"ovn-empty-lb-events"`
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

// DefaultMetricsCollectionInterval is the default interval, in seconds, at which
// OVS/OVN metric values are extracted into the registry when
// MetricsConfig.CollectionInterval is unset or non-positive.
const DefaultMetricsCollectionInterval = 30 * time.Second

// MetricsConfig holds Prometheus metrics-related parameters.
type MetricsConfig struct {
	BindAddress           string `gcfg:"bind-address"`
	OVNMetricsBindAddress string `gcfg:"ovn-metrics-bind-address"`
	ExportOVSMetrics      bool   `gcfg:"export-ovs-metrics"`
	EnablePprof           bool   `gcfg:"enable-pprof"`
	NodeServerPrivKey     string `gcfg:"node-server-privkey"`
	NodeServerCert        string `gcfg:"node-server-cert"`
	// EnableConfigDuration holds the boolean flag to enable ovnkube controller to monitor ovnkube controller
	// configuration duration and optionally, its application to all nodes
	EnableConfigDuration bool `gcfg:"enable-config-duration"`
	EnableScaleMetrics   bool `gcfg:"enable-scale-metrics"`
	// CollectionInterval is how often (in seconds) OVS/OVN metric values are
	// extracted into the registry, independent of Prometheus scrapes. A
	// non-positive value uses DefaultMetricsCollectionInterval.
	CollectionInterval int `gcfg:"collection-interval"`
}

// TLSConfig holds TLS-related configuration parameters.
type TLSConfig struct {
	MinVersion   string `gcfg:"tls-min-version"`
	CipherSuites string `gcfg:"tls-cipher-suites"`

	// ApplyOptions is the parsed and validated TLS configuration function
	// that can be applied to tls.Config. This is populated during config
	// initialization and should not be set via config file or CLI.
	ApplyOptions tls.ApplyConfigOptions
}

// ParseCipherSuites parses the comma-separated CipherSuites string into a slice.
// Trims whitespace and filters out empty strings.
func (c *TLSConfig) ParseCipherSuites() []string {
	suites := strings.Split(c.CipherSuites, ",")
	result := make([]string, 0, len(suites))
	for _, suite := range suites {
		if trimmed := strings.TrimSpace(suite); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
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
	EnableNetworkSegmentation       bool `gcfg:"enable-network-segmentation"`
	EnableNetworkConnect            bool `gcfg:"enable-network-connect"`
	EnableUplink                    bool `gcfg:"enable-uplink"`
	EnablePreconfiguredUDNAddresses bool `gcfg:"enable-preconfigured-udn-addresses"`
	EnableRouteAdvertisements       bool `gcfg:"enable-route-advertisements"`
	EnableEVPN                      bool `gcfg:"enable-evpn"`
	EnableMultiNetworkPolicy        bool `gcfg:"enable-multi-networkpolicy"`
	EnableStatelessNetPol           bool `gcfg:"enable-stateless-netpol"`
	EnableMultiExternalGateway      bool `gcfg:"enable-multi-external-gateway"`
	EnablePersistentIPs             bool `gcfg:"enable-persistent-ips"`
	EnableDNSNameResolver           bool `gcfg:"enable-dns-name-resolver"`
	EnableServiceTemplateSupport    bool `gcfg:"enable-svc-template-support"`
	EnableObservability             bool `gcfg:"enable-observability"`
	EnableNetworkQoS                bool `gcfg:"enable-network-qos"`
	AllowICMPNetworkPolicy          bool `gcfg:"allow-icmp-network-policy"`
	// This feature requires a kernel fix https://github.com/torvalds/linux/commit/7f3287db654395f9c5ddd246325ff7889f550286
	// to work on a kind cluster. Flag allows to disable it for current CI, will be turned on when github runners have this fix.
	AdvertisedUDNIsolationMode string `gcfg:"advertised-udn-isolation-mode"`
	EnableDynamicUDNAllocation bool   `gcfg:"enable-dynamic-udn-allocation"`
	// UDNDeletionGracePeriod specified in number of seconds to wait before garbage collecting a UDN. Applies
	// only when Dynamic UDN Allocation is enabled.
	UDNDeletionGracePeriod time.Duration `gcfg:"udn-deletion-grace-period"`
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

const (
	// AdvertisedUDNIsolationModeStrict pod isolation across advertised UDN networks is enabled.
	AdvertisedUDNIsolationModeStrict = "strict"
	// AdvertisedUDNIsolationModeLoose pod isolation across advertised UDN networks is disabled.
	AdvertisedUDNIsolationModeLoose = "loose"
)

// GatewayConfig holds node gateway-related parsed config file parameters and command-line overrides
type GatewayConfig struct {
	// Mode is the gateway mode; if may be either empty (disabled), "shared", or "local"
	Mode GatewayMode `gcfg:"mode"`
	// Interface is the network interface to use for the gateway in "shared" mode
	Interface string `gcfg:"interface"`
	// GatewayAcceleratedInterface is the optional network interface to use for gateway traffic acceleration.
	// This is typically a VF or SF device. When specified it would be used as the in_port for Openflow rules
	// on the external bridge. The Host IP would be on this device.
	// Should be used mutually exclusive to the `--gateway-interface` flag.
	GatewayAcceleratedInterface string `gcfg:"gateway-accelerated-interface"`
	// DPUHostGatewayRepresentorInterface is the DPU-side representor of the host's
	// uplink (PF). For some DPUs this is discovered automatically via
	// phys_port_name via switchdev metadata. In simulated environments or other
	// interpretations of DPUs, it must be specified explicitly
	// because the interface has no switchdev metadata.
	DPUHostGatewayRepresentorInterface string `gcfg:"dpu-host-gateway-representor-interface"`
	// Egress gateway interface is the optional network interface to use for external gw pods traffic.
	EgressGWInterface string `gcfg:"egw-interface"`
	// NextHop is the gateway IP address of Interface; will be autodetected if not given
	NextHop string `gcfg:"next-hop"`
	// VLANID is the option VLAN tag to apply to gateway traffic for "shared" mode
	VLANID uint `gcfg:"vlan-id"`
	// NodeportEnable sets whether to provide Kubernetes NodePort service or not
	NodeportEnable bool `gcfg:"nodeport"`
	// DisableSNATMultipleGws sets whether to disable SNAT of egress traffic in namespaces with external gateways configured via AdminPolicyBasedExternalRoute CRs
	// only applicable to the default network not for UDNs
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
	// DisableForwarding controls if IPv6 forwarding is blocked on non-OVNK controlled interfaces when using older kernels
	DisableForwarding bool `gcfg:"disable-forwarding"`
	// AllowNoUplink (disabled by default) controls if the external gateway bridge without an uplink port is allowed in local gateway mode.
	AllowNoUplink bool `gcfg:"allow-no-uplink"`
	// EphemeralPortRange is the range of ports used by egress SNAT operations in OVN. Specifically for NAT where
	// the source IP of the NAT will be a shared Node IP address. If unset, the value will be determined by sysctl lookup
	// for the kernel's ephemeral range: net.ipv4.ip_local_port_range. Format is "<min port>-<max port>".
	EphemeralPortRange string `gcfg:"ephemeral-port-range"`

	// DisableUDNARPNDPFlood might provide better scale capabilities with large
	// number of UDNs. Instead of flooding ARP/NDP traffic from the gateway to
	// the UDNs which might overload vswitchd or even drop packets, only CDN or
	// UDN in an uplink group will get the traffic, OVNK will mirror the
	// resulting mac binding entries to the UDNs.
	DisableUDNARPNDPFlood bool `gcfg:"disable-udn-arp-ndp-flood"`
}

// EgressIPHealthCheckTLSConfig holds TLS settings for the Egress IP gRPC
// health-check channel.
type EgressIPHealthCheckTLSConfig struct {
	PrivKey        string `gcfg:"client-privkey"`
	Cert           string `gcfg:"client-cert"`
	CACert         string `gcfg:"client-cacert"`
	CertCommonName string `gcfg:"cert-common-name"`
}

// OvnDBConfig holds local OVN database paths. OVN databases are accessed
// through Unix sockets, whose addresses are derived from RunDir.
type OvnDBConfig struct {
	northbound bool
	// RunDir is OVN run directory.
	RunDir string `gcfg:"run-dir"`
	// DbLocation is OVN northbound/southbound database location.
	DbLocation string `gcfg:"db-location"`

	exec kexec.Interface
}

// legacyOvnNorthConfig preserves the historical [ovnnorth] config-file layout
// as a legacy input. New configurations should use
// [egressip-healthcheck-tls] for Egress IP health-check TLS settings. Runtime
// state keeps the concerns separate.
type legacyOvnNorthConfig struct {
	OvnDBConfig
	EgressIPHealthCheckTLSConfig
}

// OvsPathConfig holds OVS location details.
type OvsPathConfig struct {
	// RunDir is OVS run directory.
	RunDir string `gcfg:"run-dir"`
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
	Mode                      string `gcfg:"mode"`
	MgmtPortNetdev            string `gcfg:"mgmt-port-netdev"`
	MgmtPortDPResourceName    string `gcfg:"mgmt-port-dp-resource-name"`
	DPUNodeLeaseRenewInterval int    `gcfg:"dpu-node-lease-renew-interval"`
	DPUNodeLeaseDuration      int    `gcfg:"dpu-node-lease-duration"`
	SimulateDPU               bool   `gcfg:"simulate-dpu"`
	// RoutingTableIDStart is added to interface indexes to derive OVN-managed Linux route table IDs.
	RoutingTableIDStart int `gcfg:"routing-table-id-start"`
	// KubeletCgroupPath is the cgroup v2 path kubelet runs under, relative to
	// /sys/fs/cgroup. When empty, the kubelet.service cgroup is looked up instead.
	KubeletCgroupPath string `gcfg:"kubelet-cgroup-path"`
}

// ClusterManagerConfig holds configuration for ovnkube-cluster-manager
type ClusterManagerConfig struct {
	// V4TransitSubnet to be used in the cluster for interconnecting multiple zones
	V4TransitSubnet string `gcfg:"v4-transit-subnet"`
	// V6TransitSubnet to be used in the cluster for interconnecting multiple zones
	V6TransitSubnet string `gcfg:"v6-transit-subnet"`
}

// NoOverlayConfig holds configuration for no-overlay mode
type NoOverlayConfig struct {
	// OutboundSNAT configures SNAT behavior for outbound traffic from pods on the default network.
	// Supported values: "enabled" or "disabled".
	// Required when transport=no-overlay.
	OutboundSNAT string `gcfg:"outbound-snat"`
	// Routing configures whether the pod network routing configuration is managed by
	// OVN-Kubernetes or users. Supported values: "managed" or "unmanaged".
	// Required when transport=no-overlay.
	Routing string `gcfg:"routing"`
}

// ManagedBGPConfig holds configuration for managed BGP
type ManagedBGPConfig struct {
	// ASNumber specifies the AS number to be used by the BGP speakers on each node for its
	// default VRF when no-overlay networks are configured with managed routing.
	// It is shared by both the cluster default network and CUDNs.
	// Supports both 16-bit (1-65535) and 32-bit (1-4294967295) AS numbers.
	// Optional. Defaults to 64512 if not specified.
	ASNumber uint32 `gcfg:"as-number"`
	// Topology configures the BGP peering topology when routing is managed.
	// Supported values: "full-mesh".
	// Required when transport=no-overlay and routing=managed.
	Topology string `gcfg:"topology"`
	// FRRNamespace specifies the namespace where FRR-K8s FRRConfiguration resources are created
	// when routing is managed.
	// In unmanaged mode, the namespace is determined automatically by detecting user-created
	// FRR-K8s FRRConfiguration resources. Therefore this flag is not needed.
	FRRNamespace string `gcfg:"frr-namespace"`
}

// Config is used to read the structured config file and to cache config in testcases
type config struct {
	Default                DefaultConfig
	Logging                LoggingConfig
	Monitoring             MonitoringConfig
	IPFIX                  IPFIXConfig
	CNI                    CNIConfig
	OVNKubernetesFeature   OVNKubernetesFeatureConfig
	Kubernetes             KubernetesConfig
	Metrics                MetricsConfig
	TLS                    TLSConfig
	EgressIPHealthCheckTLS EgressIPHealthCheckTLSConfig `gcfg:"egressip-healthcheck-tls"`
	OvnNorth               legacyOvnNorthConfig
	OvnSouth               OvnDBConfig
	Gateway                GatewayConfig
	ClusterMgrHA           HAConfig
	HybridOverlay          HybridOverlayConfig
	OvnKubeNode            OvnKubeNodeConfig
	ClusterManager         ClusterManagerConfig
	OvsPaths               OvsPathConfig
	NoOverlay              NoOverlayConfig  `gcfg:"no-overlay"`
	ManagedBGP             ManagedBGPConfig `gcfg:"bgp-managed"`
}

var (
	savedDefault                DefaultConfig
	savedLogging                LoggingConfig
	savedMonitoring             MonitoringConfig
	savedIPFIX                  IPFIXConfig
	savedCNI                    CNIConfig
	savedOVNKubernetesFeature   OVNKubernetesFeatureConfig
	savedKubernetes             KubernetesConfig
	savedMetrics                MetricsConfig
	savedTLS                    TLSConfig
	savedOvnNorth               OvnDBConfig
	savedOvnSouth               OvnDBConfig
	savedEgressIPHealthCheckTLS EgressIPHealthCheckTLSConfig
	savedGateway                GatewayConfig
	savedClusterMgrHA           HAConfig
	savedHybridOverlay          HybridOverlayConfig
	savedOvnKubeNode            OvnKubeNodeConfig
	savedClusterManager         ClusterManagerConfig
	savedOvsPaths               OvsPathConfig
	savedNoOverlay              NoOverlayConfig
	savedManagedBGP             ManagedBGPConfig

	// legacy service-cluster-ip-range CLI option
	serviceClusterIPRange string
	// legacy cluster-subnet CLI option
	clusterSubnet string
	// legacy init-gateways CLI option
	initGateways bool
	// legacy gateway-local CLI option
	gatewayLocal bool
)

func init() {
	// ClusterDefaultNetworkNAD is normally resolved in completeDefaultConfig, but
	// unit tests rely on PrepareTestConfig instead of the full init flow. Seed a
	// valid default here, before savedDefault is captured below, so it is never
	// nil and PrepareTestConfig restores a usable value for those tests.
	// ClusterDefaultNADName is deliberately left empty so that savedDefault
	// records it as unset and CLI/file overrides are still detected.
	Default.ClusterDefaultNetworkNAD, _ = parseClusterDefaultNAD(defaultClusterDefaultNADName())

	// Cache original default config values
	savedDefault = Default
	savedLogging = Logging
	savedMonitoring = Monitoring
	savedIPFIX = IPFIX
	savedCNI = CNI
	savedOVNKubernetesFeature = OVNKubernetesFeature
	savedKubernetes = Kubernetes
	savedMetrics = Metrics
	savedTLS = TLS
	savedOvnNorth = OvnNorth
	savedOvnSouth = OvnSouth
	savedEgressIPHealthCheckTLS = EgressIPHealthCheckTLS
	savedGateway = Gateway
	savedClusterMgrHA = ClusterMgrHA
	savedHybridOverlay = HybridOverlay
	savedOvnKubeNode = OvnKubeNode
	savedClusterManager = ClusterManager
	savedOvsPaths = OvsPaths
	savedNoOverlay = NoOverlay
	savedManagedBGP = ManagedBGP
	cli.VersionPrinter = func(_ *cli.Context) {
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
	TLS = savedTLS
	OvnNorth = savedOvnNorth
	OvnSouth = savedOvnSouth
	EgressIPHealthCheckTLS = savedEgressIPHealthCheckTLS
	Gateway = savedGateway
	HybridOverlay = savedHybridOverlay
	OvnKubeNode = savedOvnKubeNode
	ClusterManager = savedClusterManager
	OvsPaths = savedOvsPaths
	NoOverlay = savedNoOverlay
	ManagedBGP = savedManagedBGP
	Kubernetes.DisableRequestedChassis = false
	EnableMulticast = false
	UnprivilegedMode = false
	Default.OVSDBTxnTimeout = 5 * time.Second
	if Gateway.Mode != GatewayModeDisabled {
		Gateway.EphemeralPortRange = DefaultEphemeralPortRange
	}
	Layer2UsesTransitRouter = true

	if err := completeConfig(); err != nil {
		return err
	}

	// set klog level here as some tests will not call InitConfig
	var level klog.Level
	if err := level.Set(strconv.Itoa(Logging.Level)); err != nil {
		return fmt.Errorf("failed to set klog log level %v", err)
	}

	// Don't pick up defaults from the environment
	os.Unsetenv("KUBECONFIG")
	os.Unsetenv("K8S_CACERT")
	os.Unsetenv("K8S_CACERT_DATA")
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
		Name:  "init-cluster-manager",
		Usage: "initialize cluster manager (but not ovnkube-controller), requires the hostname as argument",
	},
	&cli.StringFlag{
		Name:  "init-ovnkube-controller",
		Usage: "initialize ovnkube-controller (but not cluster-manager), requires the Kubernetes node name as argument",
	},
	&cli.StringFlag{
		Name:  "init-node",
		Usage: "initialize node, requires the name that node is registered with in kubernetes cluster",
	},
	&cli.StringFlag{
		Name:  "zone",
		Usage: "DEPRECATED; no longer used and ignored. Will be removed in 2 releases.",
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
		Usage:       "MTU value used for the overlay networks",
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
		Usage:       "For gateway nodes, the conntrack zone used for conntrack flow rules",
		Destination: &cliConfig.Default.ConntrackZone,
		Value:       Default.ConntrackZone,
	},
	&cli.StringFlag{
		Name:        "encap-type",
		Usage:       "The encapsulation protocol to use to transmit packets between hypervisors by OVN in overlay mode (geneve, vxlan, gre)",
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
		Usage:       "The UDP port used by the encapsulation endpoint",
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
	&cli.DurationFlag{
		Name: "db-txn-timeout",
		Usage: "OVSDBTxnTimeout is the timeout for db transaction in seconds, " +
			"may be useful to increase for high-scale clusters.",
		Destination: &cliConfig.Default.OVSDBTxnTimeout,
		Value:       Default.OVSDBTxnTimeout,
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
	&cli.StringFlag{
		Name:        "transport",
		Value:       Default.Transport,
		Usage:       "Transport technology for the default network. When unset, the OVN default overlay transport is used. (no-overlay)",
		Destination: &cliConfig.Default.Transport,
	},
	&cli.StringFlag{
		Name:  "cluster-default-nad",
		Value: Default.ClusterDefaultNADName,
		Usage: "Namespace/name of the default cluster network NAD. " +
			"When unset, defaults to <ovn-config-namespace>/" + types.DefaultNetworkName + ".",
		Destination: &cliConfig.Default.ClusterDefaultNADName,
	},
	&cli.BoolFlag{
		Name:        "unprivileged-mode",
		Usage:       "Run ovnkube-node container in unprivileged mode. Valid only with --init-node option.",
		Destination: &UnprivilegedMode,
	},
	&cli.BoolFlag{
		Name:        "enable-multicast",
		Usage:       "Adds multicast support. Valid only with ovnkube-controller mode.",
		Destination: &EnableMulticast,
	},
	// Logging options
	&cli.IntFlag{
		Name:        "loglevel",
		Usage:       "log verbosity and level: info, warn, fatal, error are always printed no matter the log level. Use 5 for debug",
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
		Usage:       "path of a file to direct log from cni shim to output to",
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
		Usage:       "Maximum size in megabytes of the log file before it gets rolled",
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
		Usage:       "The largest number of messages per second that gets logged before drop",
		Destination: &cliConfig.Logging.ACLLoggingRateLimit,
		Value:       20,
	},
	&cli.StringFlag{
		Name: "udn-allowed-default-services",
		Usage: "a list of namespaced names of default cluster network services accessible from primary" +
			"user-defined networks. If not specified defaults to [\"default/kubernetes\", \"kube-system/kube-dns\"]." +
			"Only used when enable-network-segmentation is set",
		Value:       Default.RawUDNAllowedDefaultServices,
		Destination: &cliConfig.Default.RawUDNAllowedDefaultServices,
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
		Usage:       "Rate at which packets should be sampled and sent to each target collector",
		Destination: &cliConfig.IPFIX.Sampling,
		Value:       IPFIX.Sampling,
	},
	&cli.UintFlag{
		Name:        "ipfix-cache-max-flows",
		Usage:       "Maximum number of IPFIX flow records that can be cached at a time. If 0, caching is disabled",
		Destination: &cliConfig.IPFIX.CacheMaxFlows,
		Value:       IPFIX.CacheMaxFlows,
	}, &cli.UintFlag{
		Name:        "ipfix-cache-active-timeout",
		Usage:       "Maximum period in seconds for which an IPFIX flow record is cached and aggregated before being sent. If 0, caching is disabled",
		Destination: &cliConfig.IPFIX.CacheActiveTimeout,
		Value:       IPFIX.CacheActiveTimeout,
	},
}

// CNIFlags capture CNI-related options
var CNIFlags = []cli.Flag{
	// CNI options
	&cli.StringFlag{
		Name:        "cni-conf-dir",
		Usage:       "the CNI config directory in which to write the overlay CNI config file",
		Destination: &cliConfig.CNI.ConfDir,
		Value:       CNI.ConfDir,
	},
	&cli.StringFlag{
		Name:        "cni-plugin",
		Usage:       "the name of the CNI plugin",
		Destination: &cliConfig.CNI.Plugin,
		Value:       CNI.Plugin,
	},
}

// OVNK8sFeatureFlags capture OVN-Kubernetes feature related options
var OVNK8sFeatureFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:        "enable-admin-network-policy",
		Usage:       "Use Admin Network Policy CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableAdminNetworkPolicy,
		Value:       OVNKubernetesFeature.EnableAdminNetworkPolicy,
	},
	&cli.BoolFlag{
		Name:        "enable-egress-ip",
		Usage:       "Use EgressIP CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableEgressIP,
		Value:       OVNKubernetesFeature.EnableEgressIP,
	},
	&cli.IntFlag{
		Name:        "egressip-reachability-total-timeout",
		Usage:       "EgressIP node reachability total timeout in seconds",
		Destination: &cliConfig.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout,
		Value:       1,
	},
	&cli.BoolFlag{
		Name:        "enable-egress-firewall",
		Usage:       "Use EgressFirewall CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableEgressFirewall,
		Value:       OVNKubernetesFeature.EnableEgressFirewall,
	},
	&cli.BoolFlag{
		Name:        "enable-egress-qos",
		Usage:       "Use EgressQoS CRD feature with ovn-kubernetes.",
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
		Usage:       "Use multiple NetworkAttachmentDefinition CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableMultiNetwork,
		Value:       OVNKubernetesFeature.EnableMultiNetwork,
	},
	&cli.BoolFlag{
		Name:        "enable-multi-networkpolicy",
		Usage:       "Use MultiNetworkPolicy CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableMultiNetworkPolicy,
		Value:       OVNKubernetesFeature.EnableMultiNetworkPolicy,
	},
	&cli.BoolFlag{
		Name:        "enable-network-segmentation",
		Usage:       "Use network segmentation feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableNetworkSegmentation,
		Value:       OVNKubernetesFeature.EnableNetworkSegmentation,
	},
	&cli.BoolFlag{
		Name:        "enable-network-connect",
		Usage:       "Configure to use network connect feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableNetworkConnect,
		Value:       OVNKubernetesFeature.EnableNetworkConnect,
	},
	&cli.BoolFlag{
		Name:        "enable-uplink",
		Usage:       "Configure to use the Uplink feature with ovn-kubernetes. Requires network segmentation.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableUplink,
		Value:       OVNKubernetesFeature.EnableUplink,
	},
	&cli.BoolFlag{
		Name:        "enable-preconfigured-udn-addresses",
		Usage:       "Enable workloads connect to user-defined network with preconfigured addresses.",
		Destination: &cliConfig.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses,
		Value:       OVNKubernetesFeature.EnablePreconfiguredUDNAddresses,
	},
	&cli.BoolFlag{
		Name:        "enable-route-advertisements",
		Usage:       "Use route advertisements feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableRouteAdvertisements,
		Value:       OVNKubernetesFeature.EnableRouteAdvertisements,
	},
	&cli.BoolFlag{
		Name:        "enable-evpn",
		Usage:       "Use EVPN feature with ovn-kubernetes. Requires route advertisements.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableEVPN,
		Value:       OVNKubernetesFeature.EnableEVPN,
	},
	&cli.StringFlag{
		Name:        "advertised-udn-isolation-mode",
		Usage:       "Use pod isolation for BGP advertised UDN networks. Valid values are 'strict' or 'loose'.",
		Destination: &cliConfig.OVNKubernetesFeature.AdvertisedUDNIsolationMode,
		Value:       OVNKubernetesFeature.AdvertisedUDNIsolationMode,
	},
	&cli.BoolFlag{
		Name:        "enable-stateless-netpol",
		Usage:       "Use stateless network policy feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableStatelessNetPol,
		Value:       OVNKubernetesFeature.EnableStatelessNetPol,
	},
	&cli.BoolFlag{
		Name:        "allow-icmp-network-policy",
		Usage:       "Allow ICMP/ICMPv6 traffic to bypass NetworkPolicy default-deny rules.",
		Destination: &cliConfig.OVNKubernetesFeature.AllowICMPNetworkPolicy,
		Value:       OVNKubernetesFeature.AllowICMPNetworkPolicy,
	},
	&cli.BoolFlag{
		Name:        "enable-egress-service",
		Usage:       "Use EgressService CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableEgressService,
		Value:       OVNKubernetesFeature.EnableEgressService,
	},
	&cli.BoolFlag{
		Name:        "enable-multi-external-gateway",
		Usage:       "Use AdminPolicyBasedExternalRoute CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableMultiExternalGateway,
		Value:       OVNKubernetesFeature.EnableMultiExternalGateway,
	},
	&cli.BoolFlag{
		Name:        "enable-persistent-ips",
		Usage:       "Use the persistent ips feature for virtualization with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnablePersistentIPs,
		Value:       OVNKubernetesFeature.EnablePersistentIPs,
	},
	&cli.BoolFlag{
		Name:        "enable-dns-name-resolver",
		Usage:       "Use DNSNameResolver CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableDNSNameResolver,
		Value:       OVNKubernetesFeature.EnableDNSNameResolver,
	},
	&cli.BoolFlag{
		Name:        "enable-svc-template-support",
		Usage:       "Use svc-template with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableServiceTemplateSupport,
		Value:       OVNKubernetesFeature.EnableServiceTemplateSupport,
	},
	&cli.BoolFlag{
		Name:        "enable-observability",
		Usage:       "Use OVN sampling with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableObservability,
		Value:       OVNKubernetesFeature.EnableObservability,
	},
	&cli.BoolFlag{
		Name:        "enable-network-qos",
		Usage:       "Use NetworkQoS CRD feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableNetworkQoS,
		Value:       OVNKubernetesFeature.EnableNetworkQoS,
	},
	&cli.BoolFlag{
		Name:        "enable-dynamic-udn-allocation",
		Usage:       "Configure to use the dynamic UDN allocation feature with ovn-kubernetes.",
		Destination: &cliConfig.OVNKubernetesFeature.EnableDynamicUDNAllocation,
		Value:       OVNKubernetesFeature.EnableDynamicUDNAllocation,
	},
	&cli.DurationFlag{
		Name: "udn-deletion-grace-period",
		Usage: "Delay time in seconds that a node will wait before removing a UDN when the dynamic UDN allocation " +
			"feature is used.",
		Destination: &cliConfig.OVNKubernetesFeature.UDNDeletionGracePeriod,
		Value:       OVNKubernetesFeature.UDNDeletionGracePeriod,
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
			"option.",
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
		Usage:       "URL of the Kubernetes API server (not required if --k8s-kubeconfig is given)",
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
		Name:        "k8s-cacert-data",
		Usage:       "the Base64 encoded Kubernetes API CA certificate data (not required if --k8s-kubeconfig is given)",
		Destination: &cliConfig.Kubernetes.CACertData,
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
			"Valid values: " + strings.Join(getSupportedPlatformTypes(), ", "),
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
		Usage:       "Enables monitoring OVN-Kubernetes ovnkube controller and OVN configuration duration",
		Destination: &cliConfig.Metrics.EnableConfigDuration,
	},
	&cli.BoolFlag{
		Name:        "metrics-enable-scale",
		Usage:       "Enables metrics related to scaling",
		Destination: &cliConfig.Metrics.EnableScaleMetrics,
	},
	&cli.IntFlag{
		Name: "metrics-collection-interval",
		Usage: fmt.Sprintf("Interval in seconds at which OVS/OVN metric values are extracted "+
			"into the registry, independent of Prometheus scrapes. Non-positive uses the "+
			"default (%fs).", DefaultMetricsCollectionInterval.Seconds()),
		Destination: &cliConfig.Metrics.CollectionInterval,
	},
}

// TLSFlags capture TLS-related options
var TLSFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "tls-min-version",
		Usage:       "Minimum TLS version supported",
		Destination: &cliConfig.TLS.MinVersion,
	},
	&cli.StringFlag{
		Name:        "tls-cipher-suites",
		Usage:       "Comma-separated list of cipher suites (TLS 1.0-1.2 only; ignored for TLS 1.3)",
		Destination: &cliConfig.TLS.CipherSuites,
	},
}

// EgressIPHealthCheckTLSFlags capture Egress IP gRPC health-check TLS options.
// Their nb-* names are retained for command-line compatibility.
var EgressIPHealthCheckTLSFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "nb-client-privkey",
		Usage:       "Private key used by node↔node Egress IP gRPC health-check channel (historical name).",
		Destination: &cliConfig.EgressIPHealthCheckTLS.PrivKey,
	},
	&cli.StringFlag{
		Name:        "nb-client-cert",
		Usage:       "Client certificate used by node↔node Egress IP gRPC health-check channel (historical name).",
		Destination: &cliConfig.EgressIPHealthCheckTLS.Cert,
	},
	&cli.StringFlag{
		Name:        "nb-client-cacert",
		Usage:       "CA certificate used by node↔node Egress IP gRPC health-check channel (historical name).",
		Destination: &cliConfig.EgressIPHealthCheckTLS.CACert,
	},
	&cli.StringFlag{
		Name: "nb-cert-common-name",
		Usage: "Common Name of the certificate used for TLS server certificate verification by " +
			"the Egress IP gRPC health-check channel. In cases where the certificate doesn't have " +
			"any SAN Extensions, this parameter should match the DNS(hostname) of the server. In " +
			"case the certificate has a SAN extension, this parameter should match one of the SAN fields.",
		Destination: &cliConfig.EgressIPHealthCheckTLS.CertCommonName,
	},
}

// OvnNBFlags capture OVN northbound database options.
var OvnNBFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "nb-run-dir",
		Usage:       "OVN northbound run directory path",
		Destination: &cliConfig.OvnNorth.OvnDBConfig.RunDir,
		Value:       OvnNorth.RunDir,
	},
	&cli.StringFlag{
		Name:        "nb-db-location",
		Usage:       "OVN northbound database file location",
		Destination: &cliConfig.OvnNorth.OvnDBConfig.DbLocation,
		Value:       OvnNorth.DbLocation,
	},
}

// OvnSBFlags capture OVN southbound database options
var OvnSBFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "sb-run-dir",
		Usage:       "OVN southbound run directory path",
		Destination: &cliConfig.OvnSouth.RunDir,
		Value:       OvnSouth.RunDir,
	},
	&cli.StringFlag{
		Name:        "sb-db-location",
		Usage:       "OVN southbound database file location",
		Destination: &cliConfig.OvnSouth.DbLocation,
		Value:       OvnSouth.DbLocation,
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
		Name: "gateway-accelerated-interface",
		Usage: "The optional network interface to use for gateway traffic acceleration. " +
			"This is typically a VF or SF device. When specified it would be used as the in_port for Openflow rules " +
			"on the external bridge. The Host IP would be on this device.",
		Destination: &cliConfig.Gateway.GatewayAcceleratedInterface,
	},
	&cli.StringFlag{
		Name: "dpu-host-gateway-representor-interface",
		Usage: "The DPU-side representor interface for the host's uplink (PF). For some DPUs this is discovered " +
			"automatically via phys_port_name via switchdev metadata. In simulated environments or other interpretations of " +
			"DPUs, it must be specified explicitly because the interface has no switchdev metadata.",
		Destination: &cliConfig.Gateway.DPUHostGatewayRepresentorInterface,
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
		Name: "disable-forwarding",
		Usage: "Disable IPv6 forwarding except on OVNK controlled interfaces when using " +
			"an older kernel that doesn't allow per-interface IPv6 forwarding.",
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
	&cli.StringFlag{
		Name: "ephemeral-port-range",
		Usage: "The port range in '<min port>-<max port>' format for OVN to use when SNAT'ing to a node IP. " +
			"This range should not collide with the node port range being used in Kubernetes. If not provided, " +
			"the default value will be derived from checking the sysctl value of net.ipv4.ip_local_port_range on the node.",
		Destination: &cliConfig.Gateway.EphemeralPortRange,
		Value:       Gateway.EphemeralPortRange,
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
	&cli.BoolFlag{
		Name: "disable-udn-arp-ndp-flood",
		Usage: "This option might provide better scale capabilities with large number of UDNs. Instead of flooding " +
			"ARP/NDP traffic from the gateway to the UDNs which might overload vswitchd or even drop packets, only CDN " +
			"or UDN in an uplink group will get the traffic, OVNK will mirror the resulting mac binding entries to the UDNs.",
		Destination: &cliConfig.Gateway.DisableUDNARPNDPFlood,
		Value:       Gateway.DisableUDNARPNDPFlood,
	},
}

// ClusterMgrHAFlags capture leader election flags for cluster manager
var ClusterMgrHAFlags = []cli.Flag{
	&cli.IntFlag{
		Name:        "cluster-manager-ha-election-lease-duration",
		Usage:       "Leader election lease duration (in secs)",
		Destination: &cliConfig.ClusterMgrHA.ElectionLeaseDuration,
		Value:       ClusterMgrHA.ElectionLeaseDuration,
	},
	&cli.IntFlag{
		Name:        "cluster-manager-ha-election-renew-deadline",
		Usage:       "Leader election renew deadline (in secs)",
		Destination: &cliConfig.ClusterMgrHA.ElectionRenewDeadline,
		Value:       ClusterMgrHA.ElectionRenewDeadline,
	},
	&cli.IntFlag{
		Name:        "cluster-manager-ha-election-retry-period",
		Usage:       "Leader election retry period (in secs)",
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
		Usage:       "ovnkube-node operating mode full (default), dpu, dpu-host",
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
	&cli.StringFlag{
		Name: "kubelet-cgroup-path",
		Usage: "The cgroup v2 path kubelet runs under, relative to /sys/fs/cgroup, for example " +
			"\"kubelet.slice/kubelet.service\". Used by UDN host isolation to allow kubelet probes to " +
			"primary UDN pods. Every process in this cgroup is allowed to reach them, so it should be " +
			"the cgroup of kubelet itself rather than a parent it shares with other workloads. " +
			"When unset, the kubelet.service cgroup is looked up instead, which requires kubelet to be " +
			"managed by systemd. Only used when enable-network-segmentation is set",
		Value:       OvnKubeNode.KubeletCgroupPath,
		Destination: &cliConfig.OvnKubeNode.KubeletCgroupPath,
	},
	&cli.IntFlag{
		Name:        "dpu-node-lease-renew-interval",
		Usage:       "Interval in seconds at which the DPU updates its custom node lease. Set to 0 to disable DPU health checking",
		Value:       OvnKubeNode.DPUNodeLeaseRenewInterval,
		Destination: &cliConfig.OvnKubeNode.DPUNodeLeaseRenewInterval,
	},
	&cli.IntFlag{
		Name:        "dpu-node-lease-duration",
		Usage:       "Lease duration in seconds before the DPU is considered unhealthy",
		Value:       OvnKubeNode.DPUNodeLeaseDuration,
		Destination: &cliConfig.OvnKubeNode.DPUNodeLeaseDuration,
	},
	&cli.BoolFlag{
		Name:        "simulate-dpu",
		Usage:       "Use simulated DPU operations instead of real SR-IOV/switchdev hardware. Required for Kind and VM-based DPU simulation environments.",
		Value:       OvnKubeNode.SimulateDPU,
		Destination: &cliConfig.OvnKubeNode.SimulateDPU,
	},
	&cli.IntFlag{
		Name: "ovnkube-node-routing-table-id-start",
		Usage: fmt.Sprintf("Start of the Linux route table ID range managed by ovnkube-node "+
			"for generated VRF and route tables. Must be between %d and %d",
			MinimumRoutingTableIDStart, MaximumRoutingTableIDStart),
		Value:       OvnKubeNode.RoutingTableIDStart,
		Destination: &cliConfig.OvnKubeNode.RoutingTableIDStart,
	},
}

// ClusterManagerFlags captures ovnkube-cluster-manager specific configurations
var ClusterManagerFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "cluster-manager-v4-transit-subnet",
		Usage:       "The v4 transit subnet used for assigning transit switch and transit router IPv4 addresses for interconnect",
		Destination: &cliConfig.ClusterManager.V4TransitSubnet,
		Value:       ClusterManager.V4TransitSubnet,
	},
	&cli.StringFlag{
		Name:        "cluster-manager-v6-transit-subnet",
		Usage:       "The v6 transit switch subnet used for assigning transit switch and transit router IPv6 addresses for interconnect",
		Destination: &cliConfig.ClusterManager.V6TransitSubnet,
		Value:       ClusterManager.V6TransitSubnet,
	},
}

// OvsPathsFlags capture OVS path configuration options
var OvsPathsFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "ovs-run-dir",
		Usage:       "OVS run directory path",
		Destination: &cliConfig.OvsPaths.RunDir,
		Value:       OvsPaths.RunDir,
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
	flags = append(flags, TLSFlags...)
	flags = append(flags, EgressIPHealthCheckTLSFlags...)
	flags = append(flags, OvnNBFlags...)
	flags = append(flags, OvnSBFlags...)
	flags = append(flags, OVNGatewayFlags...)
	flags = append(flags, ClusterMgrHAFlags...)
	flags = append(flags, HybridOverlayFlags...)
	flags = append(flags, MonitoringFlags...)
	flags = append(flags, IPFIXFlags...)
	flags = append(flags, OvnKubeNodeFlags...)
	flags = append(flags, ClusterManagerFlags...)
	flags = append(flags, OvsPathsFlags...)
	flags = append(flags, customFlags...)
	return flags
}

// Defaults are a set of flags to indicate which options should be read from
// ovs-vsctl and used as default values if option is not found via the config
// file or command-line
type Defaults struct {
	K8sAPIServer bool
	K8sToken     bool
	K8sTokenFile bool
	K8sCert      bool
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

// reconcileKubernetesAuthFields ensures that if a config stage provides Token/TokenFile
// or CACert/CACertData, stale value for any of these set by previous stage is cleared.
// This is required since any combination of these fields could be set by any stage
// and might get overwritten only partially.
func reconcileKubernetesAuthFields(k *KubernetesConfig, override *KubernetesConfig) {
	// If this stage provided either Token or TokenFile, clear the other field
	// not provided by this stage.
	overrideHasToken := override.Token != ""
	overrideHasTokenFile := override.TokenFile != ""

	if overrideHasToken || overrideHasTokenFile {
		if !overrideHasToken {
			k.Token = ""
		}
		if !overrideHasTokenFile {
			k.TokenFile = ""
		}
	}

	// If this stage provided either CACert or CACertData, clear the other field
	// not provided by this stage.
	overrideHasCACert := override.CACert != ""
	overrideHasCACertData := override.CACertData != ""

	if overrideHasCACert || overrideHasCACertData {
		if !overrideHasCACert {
			k.CACert = ""
		}
		if !overrideHasCACertData {
			k.CACertData = ""
		}
	}
}

func buildKubernetesConfig(exec kexec.Interface, cli, file *config, saPath string, defaults *Defaults) error {
	// values for token, cacert, kubeconfig, api-server may be found in several places.
	// Priority order (highest first): OVS config, command line options, config file,
	// environment variables, service account files

	// token and ca.crt may be from files mounted in container.
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

	envConfig := savedKubernetes
	envVarsMap := map[string]string{
		"Kubeconfig":           "KUBECONFIG",
		"BootstrapKubeconfig":  "BOOTSTRAP_KUBECONFIG",
		"CertDir":              "CERT_DIR",
		"CACert":               "K8S_CACERT",
		"CACertData":           "K8S_CACERT_DATA",
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
	reconcileKubernetesAuthFields(&Kubernetes, &envConfig)

	// Copy config file values over default values
	if err := overrideFields(&Kubernetes, &file.Kubernetes, &savedKubernetes); err != nil {
		return err
	}
	reconcileKubernetesAuthFields(&Kubernetes, &file.Kubernetes)

	// And CLI overrides over config file and default values
	if err := overrideFields(&Kubernetes, &cli.Kubernetes, &savedKubernetes); err != nil {
		return err
	}
	reconcileKubernetesAuthFields(&Kubernetes, &cli.Kubernetes)

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

	if Kubernetes.CACert != "" || Kubernetes.CACertData != "" {
		var bytes []byte
		var err error
		if Kubernetes.CACert != "" {
			bytes, err = os.ReadFile(Kubernetes.CACert)
		} else {
			bytes, err = base64.StdEncoding.DecodeString(Kubernetes.CACertData)
		}

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
func completeKubernetesConfig(allSubnets *ConfigSubnets) error {
	Kubernetes.ServiceCIDRs = []*net.IPNet{}
	for _, cidrString := range strings.Split(Kubernetes.RawServiceCIDRs, ",") {
		_, serviceCIDR, err := net.ParseCIDR(cidrString)
		if err != nil {
			return fmt.Errorf("kubernetes service network CIDR %q invalid: %v", cidrString, err)
		}
		Kubernetes.ServiceCIDRs = append(Kubernetes.ServiceCIDRs, serviceCIDR)
		allSubnets.Append(ConfigSubnetService, serviceCIDR)
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

func buildTLSConfig(cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&TLS, &file.TLS, &savedTLS); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&TLS, &cli.TLS, &savedTLS); err != nil {
		return err
	}

	// Parse and validate the TLS config, storing the result
	var err error
	TLS.ApplyOptions, err = tls.NewApplyConfigOptions(TLS.MinVersion, TLS.ParseCipherSuites())
	if err != nil {
		return fmt.Errorf("invalid TLS configuration: %v", err)
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

		if len(Gateway.EphemeralPortRange) > 0 {
			if !isValidEphemeralPortRange(Gateway.EphemeralPortRange) {
				return fmt.Errorf("invalid ephemeral-port-range, should be in the format <min port>-<max port>")
			}
		} else {
			// auto-detect ephermal range
			portRange, err := getKernelEphemeralPortRange()
			if err != nil {
				return fmt.Errorf("unable to auto-detect ephemeral port range to use with OVN")
			}
			Gateway.EphemeralPortRange = portRange
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
		if len(Gateway.EphemeralPortRange) > 0 {
			return fmt.Errorf("gateway ephemeral port range option not allowed when gateway is disabled")
		}
	}

	if Gateway.Mode != GatewayModeShared && Gateway.VLANID != 0 {
		return fmt.Errorf("gateway VLAN ID option: %d is supported only in shared gateway mode", Gateway.VLANID)
	}

	return nil
}

func completeGatewayConfig(allSubnets *ConfigSubnets, masqueradeIPs *MasqueradeIPsConfig) error {
	// Validate v4 and v6 join subnets
	v4IP, v4JoinCIDR, err := net.ParseCIDR(Gateway.V4JoinSubnet)
	if err != nil || utilnet.IsIPv6(v4IP) {
		return fmt.Errorf("invalid gateway v4 join subnet specified, subnet: %s: error: %v", Gateway.V4JoinSubnet, err)
	}

	v6IP, v6JoinCIDR, err := net.ParseCIDR(Gateway.V6JoinSubnet)
	if err != nil || !utilnet.IsIPv6(v6IP) {
		return fmt.Errorf("invalid gateway v6 join subnet specified, subnet: %s: error: %v", Gateway.V6JoinSubnet, err)
	}
	allSubnets.Append(ConfigSubnetJoin, v4JoinCIDR)
	allSubnets.Append(ConfigSubnetJoin, v6JoinCIDR)

	//validate v4 and v6 masquerade subnets
	v4MasqueradeIP, v4MasqueradeCIDR, err := net.ParseCIDR(Gateway.V4MasqueradeSubnet)
	if err != nil || utilnet.IsIPv6(v4MasqueradeCIDR.IP) {
		return fmt.Errorf("invalid gateway v4 masquerade subnet specified, subnet: %s: error: %v", Gateway.V4MasqueradeSubnet, err)
	}
	if err = AllocateV4MasqueradeIPs(v4MasqueradeIP, masqueradeIPs); err != nil {
		return fmt.Errorf("unable to allocate V4MasqueradeIPs: %s", err)
	}

	v6MasqueradeIP, v6MasqueradeCIDR, err := net.ParseCIDR(Gateway.V6MasqueradeSubnet)
	if err != nil || !utilnet.IsIPv6(v6MasqueradeCIDR.IP) {
		return fmt.Errorf("invalid gateway v6 masquerade subnet specified, subnet: %s: error: %v", Gateway.V6MasqueradeSubnet, err)
	}
	if err = AllocateV6MasqueradeIPs(v6MasqueradeIP, masqueradeIPs); err != nil {
		return fmt.Errorf("unable to allocate V6MasqueradeIPs: %s", err)
	}

	allSubnets.Append(ConfigSubnetMasquerade, v4MasqueradeCIDR)
	allSubnets.Append(ConfigSubnetMasquerade, v6MasqueradeCIDR)

	// Precompute the set consumed by IsGatewayStaticMACBindingIP now that the
	// masquerade IPs are final; this is the only place they are finalized.
	gatewayStaticMACBindingIPs = sets.New[string]()
	for _, ip := range []net.IP{
		masqueradeIPs.V4DummyNextHopMasqueradeIP,
		masqueradeIPs.V6DummyNextHopMasqueradeIP,
		masqueradeIPs.V4HostMasqueradeIP,
		masqueradeIPs.V6HostMasqueradeIP,
	} {
		if ip != nil {
			gatewayStaticMACBindingIPs.Insert(ip.String())
		}
	}

	return nil
}

// gatewayStaticMACBindingIPs is the precomputed set of masquerade IP strings
// the gateway programs as static MAC bindings on gateway router external ports,
// populated by completeGatewayConfig. Read it through
// IsGatewayStaticMACBindingIP.
var gatewayStaticMACBindingIPs sets.Set[string]

// IsGatewayStaticMACBindingIP reports whether ip is one of the masquerade IPs
// the gateway programs as static MAC bindings on gateway router external ports
// (the dummy next-hop and host masquerade IPs).
func IsGatewayStaticMACBindingIP(ip string) bool {
	return gatewayStaticMACBindingIPs.Has(ip)
}

func buildOVNKubernetesFeatureConfig(cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&OVNKubernetesFeature, &file.OVNKubernetesFeature, &savedOVNKubernetesFeature); err != nil {
		return err
	}
	// And CLI overrides over config file and default values
	if err := overrideFields(&OVNKubernetesFeature, &cli.OVNKubernetesFeature, &savedOVNKubernetesFeature); err != nil {
		return err
	}
	if OVNKubernetesFeature.AdvertisedUDNIsolationMode != AdvertisedUDNIsolationModeStrict && OVNKubernetesFeature.AdvertisedUDNIsolationMode != AdvertisedUDNIsolationModeLoose {
		return fmt.Errorf("invalid advertised-udn-isolation-mode %q: expect one of %s or %s",
			OVNKubernetesFeature.AdvertisedUDNIsolationMode, AdvertisedUDNIsolationModeStrict, AdvertisedUDNIsolationModeLoose)
	}
	if OVNKubernetesFeature.EnableEVPN && !OVNKubernetesFeature.EnableRouteAdvertisements {
		return fmt.Errorf("invalid feature configuration: EVPN requires route advertisements but route advertisements are disabled")
	}
	if OVNKubernetesFeature.EnableDynamicUDNAllocation && !OVNKubernetesFeature.EnableNetworkSegmentation {
		return fmt.Errorf("the Dynamic UDN Allocation feature cannot be enabled without also enabling Network Segmentation")
	}
	if OVNKubernetesFeature.EnableUplink && !OVNKubernetesFeature.EnableNetworkSegmentation {
		return fmt.Errorf("the Uplink feature cannot be enabled without also enabling Network Segmentation")
	}
	return nil
}

func buildClusterMgrHAConfig(cli, file *config) error {
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

func buildMonitoringConfig(cli, file *config) error {
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

func buildHybridOverlayConfig(cli, file *config) error {
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
func completeHybridOverlayConfig(allSubnets *ConfigSubnets) error {
	if !HybridOverlay.Enabled || len(HybridOverlay.RawClusterSubnets) == 0 {
		return nil
	}

	var err error
	HybridOverlay.ClusterSubnets, err = ParseClusterSubnetEntries(HybridOverlay.RawClusterSubnets)
	if err != nil {
		return fmt.Errorf("hybrid overlay cluster subnet invalid: %v", err)
	}
	for _, subnet := range HybridOverlay.ClusterSubnets {
		allSubnets.Append(ConfigSubnetHybrid, subnet.CIDR)
	}

	return nil
}

func buildClusterManagerConfig(cli, file *config) error {
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

// buildNoOverlayConfig updates NoOverlay config from config file only
// NoOverlay configuration is only available in config file, not via CLI flags
func buildNoOverlayConfig(file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&NoOverlay, &file.NoOverlay, &savedNoOverlay); err != nil {
		return err
	}

	return nil
}

// validateNoOverlayConfig validates the no-overlay configuration
func validateNoOverlayConfig() error {
	// Validate transport option; empty string means default OVN overlay transport
	if Default.Transport != "" && Default.Transport != types.NetworkTransportNoOverlay {
		return fmt.Errorf("invalid transport %q: must be empty (default OVN overlay) or %q", Default.Transport, types.NetworkTransportNoOverlay)
	}

	// If transport is no-overlay, validate required no-overlay options
	if Default.Transport == types.NetworkTransportNoOverlay {
		if !OVNKubernetesFeature.EnableRouteAdvertisements {
			return fmt.Errorf("enable-route-advertisements must be true when transport=%q", types.NetworkTransportNoOverlay)
		}
		if NoOverlay.OutboundSNAT == "" {
			return fmt.Errorf("outbound-snat is required when transport=no-overlay")
		}
		if NoOverlay.OutboundSNAT != types.NoOverlaySNATEnabled && NoOverlay.OutboundSNAT != types.NoOverlaySNATDisabled {
			return fmt.Errorf("invalid outbound-snat %q: must be %q or %q", NoOverlay.OutboundSNAT, types.NoOverlaySNATEnabled, types.NoOverlaySNATDisabled)
		}

		if NoOverlay.Routing == "" {
			return fmt.Errorf("routing is required when transport=no-overlay")
		}
		if NoOverlay.Routing != NoOverlayRoutingManaged && NoOverlay.Routing != NoOverlayRoutingUnmanaged {
			return fmt.Errorf("invalid routing %q: must be %q or %q", NoOverlay.Routing, NoOverlayRoutingManaged, NoOverlayRoutingUnmanaged)
		}

		// If routing is managed, topology is required
		if NoOverlay.Routing == NoOverlayRoutingManaged {
			if ManagedBGP.Topology == "" {
				return fmt.Errorf("topology is required when routing=managed")
			}
			if ManagedBGP.Topology != ManagedBGPTopologyFullMesh {
				return fmt.Errorf("invalid topology %q: must be %q", ManagedBGP.Topology, ManagedBGPTopologyFullMesh)
			}
			if errs := validation.ValidateNamespaceName(ManagedBGP.FRRNamespace, false); len(errs) != 0 {
				return fmt.Errorf("invalid frr-namespace %q: %s", ManagedBGP.FRRNamespace, strings.Join(errs, ", "))
			}
		}
	} else {
		// Warn if no-overlay or BGP config is specified but transport is not no-overlay
		if NoOverlay.OutboundSNAT != "" || NoOverlay.Routing != "" {
			klog.Warningf("[no-overlay] configuration specified but transport is %q; configuration will be ignored", Default.Transport)
		}
	}

	return nil
}

// validateConfig performs all configuration validations after configs are built and completed.
// This is the centralized place called after completeConfig() that orchestrates all validations.
func validateConfig() error {
	// Validate managed BGP configuration
	if err := validateManagedBGPConfig(); err != nil {
		return err
	}

	// Validate no-overlay/transport configuration
	if err := validateNoOverlayConfig(); err != nil {
		return err
	}

	// Validate gateway configuration
	if err := validateGatewayConfig(); err != nil {
		return err
	}

	return nil
}

// validateGatewayConfig validates cross-configuration dependencies of the gateway
// configuration.
func validateGatewayConfig() error {
	// disable-udn-arp-ndp-flood relies on the MAC binding controller mirroring
	// bindings to UDN gateway routers, which only exist with Network Segmentation.
	if Gateway.DisableUDNARPNDPFlood && !OVNKubernetesFeature.EnableNetworkSegmentation {
		return fmt.Errorf("disable-udn-arp-ndp-flood cannot be enabled without also enabling Network Segmentation")
	}
	// The gateway bridge flows and the MAC binding controller the feature relies
	// on run where OVN and OVS do (full and DPU modes), not on the DPU host.
	if Gateway.DisableUDNARPNDPFlood && IsModeDPUHost() {
		return fmt.Errorf("disable-udn-arp-ndp-flood is not supported in DPU host mode")
	}

	return nil
}

// buildManagedBGPConfig updates managed BGP config from config file only
// ManagedBGP configuration is only available in config file, not via CLI flags
func buildManagedBGPConfig(file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&ManagedBGP, &file.ManagedBGP, &savedManagedBGP); err != nil {
		return err
	}

	return nil
}

// validateManagedBGPConfig validates the managed BGP configuration
func validateManagedBGPConfig() error {
	// Validate AS number is in valid range
	// Valid AS numbers: 1-4294967295 (32-bit)
	// Reserved ranges:
	//   0                    - Reserved (RFC 7607)
	//   23456                - AS_TRANS (RFC 6793)
	//   65535                - Reserved (RFC 7300)
	//   4294967295           - Reserved (RFC 7300)

	if ManagedBGP.ASNumber == 0 {
		return fmt.Errorf("invalid as-number: 0 is reserved")
	}
	if ManagedBGP.ASNumber == 23456 {
		return fmt.Errorf("invalid as-number: 23456 is reserved (AS_TRANS for 16-bit to 32-bit AS translation)")
	}
	if ManagedBGP.ASNumber == 65535 {
		return fmt.Errorf("invalid as-number: 65535 is reserved")
	}
	if ManagedBGP.ASNumber == 4294967295 {
		return fmt.Errorf("invalid as-number: 4294967295 is reserved")
	}

	return nil
}

// completeClusterManagerConfig completes the ClusterManager config by parsing raw values
// into their final form.
func completeClusterManagerConfig(allSubnets *ConfigSubnets) error {
	// Validate v4 and v6 transit switch subnets
	v4IP, v4TransitCIDR, err := net.ParseCIDR(ClusterManager.V4TransitSubnet)
	if err != nil || utilnet.IsIPv6(v4IP) {
		return fmt.Errorf("invalid transit switch v4 subnet specified, subnet: %s: error: %v", ClusterManager.V4TransitSubnet, err)
	}

	v6IP, v6TransitCIDR, err := net.ParseCIDR(ClusterManager.V6TransitSubnet)
	if err != nil || !utilnet.IsIPv6(v6IP) {
		return fmt.Errorf("invalid transit switch v6 subnet specified, subnet: %s: error: %v", ClusterManager.V6TransitSubnet, err)
	}
	allSubnets.Append(ConfigSubnetTransit, v4TransitCIDR)
	allSubnets.Append(ConfigSubnetTransit, v6TransitCIDR)
	return nil
}

func buildOvsPathsConfig(cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&OvsPaths, &file.OvsPaths, &savedOvsPaths); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&OvsPaths, &cli.OvsPaths, &savedOvsPaths); err != nil {
		return err
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

	return nil
}

// completeDefaultConfig completes the Default config by parsing raw values
// into their final form.
func completeDefaultConfig(allSubnets *ConfigSubnets) error {
	var err error
	Default.ClusterSubnets, err = ParseClusterSubnetEntries(Default.RawClusterSubnets)
	if err != nil {
		return fmt.Errorf("cluster subnet invalid: %v", err)
	}
	for _, subnet := range Default.ClusterSubnets {
		allSubnets.Append(ConfigSubnetCluster, subnet.CIDR)
	}

	Default.UDNAllowedDefaultServices, err = parseServicesNamespacedNames(Default.RawUDNAllowedDefaultServices)
	if err != nil {
		return fmt.Errorf("UDN allowed services field is invalid: %v", err)
	}

	Default.HostMasqConntrackZone = Default.ConntrackZone + 1
	Default.OVNMasqConntrackZone = Default.ConntrackZone + 2
	Default.HostNodePortConntrackZone = Default.ConntrackZone + 3
	Default.ReassemblyConntrackZone = Default.ConntrackZone + 4

	// Resolve here rather than at package init so that the fallback picks up
	// --ovn-config-namespace, which is only merged into Kubernetes by
	// buildKubernetesConfig earlier in InitConfig.
	if Default.ClusterDefaultNADName == "" {
		Default.ClusterDefaultNADName = defaultClusterDefaultNADName()
	}
	Default.ClusterDefaultNetworkNAD, err = parseClusterDefaultNAD(Default.ClusterDefaultNADName)
	if err != nil {
		return err
	}
	return nil
}

// defaultClusterDefaultNADName returns the location of the default cluster
// network NAD when cluster-default-nad is not configured: the default network
// NAD in the namespace ovn-kubernetes itself runs in.
func defaultClusterDefaultNADName() string {
	return Kubernetes.OVNConfigNamespace + "/" + types.DefaultNetworkName
}

// parseClusterDefaultNAD parses a "namespace/name" cluster-default-nad value
// into a NetworkSelectionElement, returning an error if it is malformed. The
// namespace and NAD name are validated as Kubernetes resource names.
func parseClusterDefaultNAD(name string) (*nettypes.NetworkSelectionElement, error) {
	nsAndName := strings.Split(name, "/")
	if len(nsAndName) != 2 || nsAndName[0] == "" || nsAndName[1] == "" {
		return nil, fmt.Errorf("cluster-default-nad %q must be in the format of \"namespace/name\"", name)
	}
	if errs := validation.ValidateNamespaceName(nsAndName[0], false); len(errs) != 0 {
		return nil, fmt.Errorf("cluster-default-nad %q has an invalid namespace: %s", name, strings.Join(errs, ", "))
	}
	if errs := validation.NameIsDNSSubdomain(nsAndName[1], false); len(errs) != 0 {
		return nil, fmt.Errorf("cluster-default-nad %q has an invalid name: %s", name, strings.Join(errs, ", "))
	}
	return &nettypes.NetworkSelectionElement{
		Namespace: nsAndName[0],
		Name:      nsAndName[1],
	}, nil
}

// parseServicesNamespacedNames splits the input string by `,` and returns a slice
// of keys that were verified to be a valid namespaced service name. It ignores spaces between the elements.
func parseServicesNamespacedNames(servicesRaw string) ([]string, error) {
	var services []string
	for _, udnEnabledSVC := range strings.Split(servicesRaw, ",") {
		svcKey := strings.TrimSpace(udnEnabledSVC)
		namespace, name, err := cache.SplitMetaNamespaceKey(strings.TrimSpace(svcKey))
		if namespace == "" {
			return nil, fmt.Errorf("UDN enabled service %q no namespace set: %v", svcKey, err)
		}
		if errs := validation.ValidateNamespaceName(namespace, false); len(errs) != 0 {
			return nil, fmt.Errorf("UDN enabled service %q has an invalid namespace: %v", svcKey, err)
		}
		if errs := validation.NameIsDNSSubdomain(name, false); len(errs) != 0 {
			return nil, fmt.Errorf("UDN enabled service %q has an invalid name: %v", svcKey, err)
		}
		services = append(services, svcKey)
	}
	return services, nil
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

// redactKubernetesConfig returns a copy of k8sConf with the bearer token and
// CA material cleared. Every log line that prints a KubernetesConfig must go
// through it, and any new field carrying inline credentials must be cleared here.
func redactKubernetesConfig(k8sConf KubernetesConfig) KubernetesConfig {
	k8sConf.Token = ""
	k8sConf.CACertData = ""
	k8sConf.CAData = []byte{}
	return k8sConf
}

// redactConfig returns a copy of cfg safe to log. The Kubernetes section is
// the only one that carries inline credentials; every other section holds
// file paths or non-secret settings.
func redactConfig(cfg config) config {
	cfg.Kubernetes = redactKubernetesConfig(cfg.Kubernetes)
	return cfg
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
		OvnNorth: legacyOvnNorthConfig{
			OvnDBConfig:                  savedOvnNorth,
			EgressIPHealthCheckTLSConfig: savedEgressIPHealthCheckTLS,
		},
		OvnSouth:               savedOvnSouth,
		EgressIPHealthCheckTLS: savedEgressIPHealthCheckTLS,
		Gateway:                savedGateway,
		ClusterMgrHA:           savedClusterMgrHA,
		HybridOverlay:          savedHybridOverlay,
		OvnKubeNode:            savedOvnKubeNode,
		ClusterManager:         savedClusterManager,
		OvsPaths:               savedOvsPaths,
		NoOverlay:              savedNoOverlay,
		ManagedBGP:             savedManagedBGP,
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
		klog.Infof("Parsed config: %+v", redactConfig(cfg))
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

	if err = buildTLSConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildOVNKubernetesFeatureConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildGatewayConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildClusterMgrHAConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildMonitoringConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildIPFIXConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildHybridOverlayConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildOvnKubeNodeConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildClusterManagerConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildOvsPathsConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildNoOverlayConfig(&cfg); err != nil {
		return "", err
	}

	if err = buildManagedBGPConfig(&cfg); err != nil {
		return "", err
	}

	tmpDBConfig, err := buildOvnDBConfig(exec, true, &cliConfig.OvnNorth.OvnDBConfig, &cfg.OvnNorth.OvnDBConfig)
	if err != nil {
		return "", err
	}
	OvnNorth = *tmpDBConfig

	tmpDBConfig, err = buildOvnDBConfig(exec, false, &cliConfig.OvnSouth, &cfg.OvnSouth)
	if err != nil {
		return "", err
	}
	OvnSouth = *tmpDBConfig

	tlsConfig, err := buildEgressIPHealthCheckTLSConfig(
		&cfg.OvnNorth.EgressIPHealthCheckTLSConfig,
		&cfg.EgressIPHealthCheckTLS,
		&cliConfig.EgressIPHealthCheckTLS,
	)
	if err != nil {
		return "", err
	}
	EgressIPHealthCheckTLS = *tlsConfig

	if err := completeConfig(); err != nil {
		return "", err
	}

	// Perform cross-configuration validations
	if err := validateConfig(); err != nil {
		return "", err
	}

	klog.V(5).Infof("Features config: %+v", OVNKubernetesFeature)
	klog.V(5).Infof("Default config: %+v", Default)
	klog.V(5).Infof("Logging config: %+v", Logging)
	klog.V(5).Infof("Monitoring config: %+v", Monitoring)
	klog.V(5).Infof("IPFIX config: %+v", IPFIX)
	klog.V(5).Infof("CNI config: %+v", CNI)
	klog.V(5).Infof("Kubernetes config: %+v", redactKubernetesConfig(Kubernetes))
	klog.V(5).Infof("Gateway config: %+v", Gateway)
	klog.V(5).Infof("OVN North config: %+v", OvnNorth)
	klog.V(5).Infof("OVN South config: %+v", OvnSouth)
	klog.V(5).Infof("Egress IP health-check TLS config: %+v", EgressIPHealthCheckTLS)
	klog.V(5).Infof("Hybrid Overlay config: %+v", HybridOverlay)
	klog.V(5).Infof("Ovnkube Node config: %+v", OvnKubeNode)
	klog.V(5).Infof("Ovnkube Cluster Manager config: %+v", ClusterManager)
	klog.V(5).Infof("OVS Paths config: %+v", OvsPaths)
	klog.V(5).Infof("No Overlay config: %+v", NoOverlay)
	klog.V(5).Infof("Managed BGP config: %+v", ManagedBGP)

	return retConfigFile, nil
}

// completeOvnKubeNodeConfig normalizes and validates the ovnkube-node configuration.
func completeOvnKubeNodeConfig() error {
	if OvnKubeNode.KubeletCgroupPath == "" {
		return nil
	}
	// cgroup paths are slash separated whatever the host, so they are not file paths.
	// ".." is rejected before cleaning, which would resolve it away.
	for _, element := range strings.Split(OvnKubeNode.KubeletCgroupPath, "/") {
		if element == ".." {
			return fmt.Errorf("kubelet cgroup path %q must not contain %q elements",
				OvnKubeNode.KubeletCgroupPath, "..")
		}
	}
	cgroupPath := strings.TrimPrefix(path.Clean("/"+OvnKubeNode.KubeletCgroupPath), "/")
	if cgroupPath == "" {
		// the cgroup root holds every process on the host, so matching on it would
		// let all of them reach primary UDN pods.
		return fmt.Errorf("kubelet cgroup path %q must not be the cgroup root",
			OvnKubeNode.KubeletCgroupPath)
	}
	OvnKubeNode.KubeletCgroupPath = cgroupPath
	return nil
}

func completeConfig() error {
	allSubnets := NewConfigSubnets()

	if err := completeOvnKubeNodeConfig(); err != nil {
		return err
	}

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

	if err := completeClusterManagerConfig(allSubnets); err != nil {
		return err
	}

	if _, _, err := allSubnets.CheckForOverlaps(); err != nil {
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

// buildOvnDBConfig returns an OvnDBConfig describing how to connect to a local
// OVN database via unix socket.
func buildOvnDBConfig(exec kexec.Interface, northbound bool, cliDB, fileDB *OvnDBConfig) (*OvnDBConfig, error) {
	var defaultConfig *OvnDBConfig
	if northbound {
		defaultConfig = &savedOvnNorth
	} else {
		defaultConfig = &savedOvnSouth
	}

	dbConfig := &OvnDBConfig{
		northbound: northbound,
		exec:       exec,
		RunDir:     defaultConfig.RunDir,
		DbLocation: defaultConfig.DbLocation,
	}

	if err := overrideFields(dbConfig, fileDB, defaultConfig); err != nil {
		return nil, err
	}
	if err := overrideFields(dbConfig, cliDB, defaultConfig); err != nil {
		return nil, err
	}

	return dbConfig, nil
}

// buildEgressIPHealthCheckTLSConfig merges the deprecated [ovnnorth] TLS
// fields, the canonical [egressip-healthcheck-tls] section, and command-line
// flags in increasing order of precedence.
func buildEgressIPHealthCheckTLSConfig(legacyFileTLS, fileTLS, cliTLS *EgressIPHealthCheckTLSConfig) (*EgressIPHealthCheckTLSConfig, error) {
	tlsConfig := savedEgressIPHealthCheckTLS
	if err := overrideFields(&tlsConfig, legacyFileTLS, &savedEgressIPHealthCheckTLS); err != nil {
		return nil, err
	}
	if err := overrideFields(&tlsConfig, fileTLS, &savedEgressIPHealthCheckTLS); err != nil {
		return nil, err
	}
	if err := overrideFields(&tlsConfig, cliTLS, &savedEgressIPHealthCheckTLS); err != nil {
		return nil, err
	}
	return &tlsConfig, nil
}

// GetURL returns the local unix-socket URL of the OVN northbound or southbound
// database, derived from RunDir.
func (a *OvnDBConfig) GetURL() string {
	direction := "sb"
	if a.northbound {
		direction = "nb"
	}
	return fmt.Sprintf("unix:%s", filepath.Join(a.RunDir, fmt.Sprintf("ovn%s_db.sock", direction)))
}

// SetOVNRemote tells ovn-controller where to find the local SB database via
// the "ovn-remote" external ID.
func (a *OvnDBConfig) SetOVNRemote() error {
	if a.northbound {
		return fmt.Errorf("cannot configure ovn-controller with the northbound database")
	}
	return setOVSExternalID(a.exec, "ovn-remote", "\""+a.GetURL()+"\"")
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

func IsModeFull() bool {
	return OvnKubeNode.Mode == types.NodeModeFull
}

func IsModeDPU() bool {
	return OvnKubeNode.Mode == types.NodeModeDPU
}

func IsModeDPUHost() bool {
	return OvnKubeNode.Mode == types.NodeModeDPUHost
}

// buildOvnKubeNodeConfig updates OvnKubeNode config from cli and config file
func buildOvnKubeNodeConfig(cli, file *config) error {
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
	if (IsModeDPU() || IsModeDPUHost()) && HybridOverlay.Enabled {
		return fmt.Errorf("hybrid overlay is not supported with ovnkube-node mode %s", OvnKubeNode.Mode)
	}

	if OvnKubeNode.DPUNodeLeaseRenewInterval < 0 {
		return fmt.Errorf("invalid dpu-node-lease-renew-interval '%d'. must be >= 0", OvnKubeNode.DPUNodeLeaseRenewInterval)
	}
	if OvnKubeNode.DPUNodeLeaseDuration <= 0 {
		return fmt.Errorf("invalid dpu-node-lease-duration '%d'. must be > 0", OvnKubeNode.DPUNodeLeaseDuration)
	}
	if OvnKubeNode.DPUNodeLeaseDuration <= OvnKubeNode.DPUNodeLeaseRenewInterval {
		return fmt.Errorf("invalid dpu-node-lease-duration '%d'. must be > dpu-node-lease-renew-interval '%d'",
			OvnKubeNode.DPUNodeLeaseDuration, OvnKubeNode.DPUNodeLeaseRenewInterval)
	}
	if OvnKubeNode.RoutingTableIDStart < MinimumRoutingTableIDStart {
		return fmt.Errorf("invalid routing-table-id-start '%d'. must be >= %d",
			OvnKubeNode.RoutingTableIDStart, MinimumRoutingTableIDStart)
	}
	if OvnKubeNode.RoutingTableIDStart > MaximumRoutingTableIDStart {
		return fmt.Errorf("invalid routing-table-id-start '%d'. must be <= %d",
			OvnKubeNode.RoutingTableIDStart, MaximumRoutingTableIDStart)
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
	if IsModeDPU() && !(OvnKubeNode.MgmtPortNetdev == "" && OvnKubeNode.MgmtPortDPResourceName == "") {
		return fmt.Errorf("ovnkube-node-mgmt-port-netdev or ovnkube-node-mgmt-port-dp-resource-name must not be provided")
	}
	if IsModeDPUHost() && OvnKubeNode.MgmtPortNetdev == "" && OvnKubeNode.MgmtPortDPResourceName == "" {
		return fmt.Errorf("ovnkube-node-mgmt-port-netdev or ovnkube-node-mgmt-port-dp-resource-name must be provided")
	}
	if OVNKubernetesFeature.EnableNetworkSegmentation && IsModeDPUHost() && OvnKubeNode.MgmtPortDPResourceName == "" {
		return fmt.Errorf("ovnkube-node-mgmt-port-dp-resource-name must be provided on dpu-host mode if network segmentation is enabled")
	}
	return nil
}
