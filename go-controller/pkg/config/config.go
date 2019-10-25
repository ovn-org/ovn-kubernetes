package config

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	gcfg "gopkg.in/gcfg.v1"

	kexec "k8s.io/utils/exec"
)

// DefaultEncapPort number used if not supplied
const DefaultEncapPort = 6081

// The following are global config parameters that other modules may access directly
var (
	// ovn-kubernetes version, to be changed with every release
	Version = "0.3.0"

	// ovn-kubernetes cni config file name
	CNIConfFileName = "10-ovn-kubernetes.conf"

	// Default holds parsed config file parameters and command-line overrides
	Default = DefaultConfig{
		MTU:               1400,
		ConntrackZone:     64000,
		EncapType:         "geneve",
		EncapIP:           "",
		EncapPort:         DefaultEncapPort,
		InactivityProbe:   100000,
		RawClusterSubnets: "10.128.0.0/14/23",
	}

	// Logging holds logging-related parsed config file parameters and command-line overrides
	Logging = LoggingConfig{
		File:  "", // do not log to a file by default
		Level: 4,
	}

	// CNI holds CNI-related parsed config file parameters and command-line overrides
	CNI = CNIConfig{
		ConfDir:         "/etc/cni/net.d",
		Plugin:          "ovn-k8s-cni-overlay",
		WinHNSNetworkID: "",
	}

	// Kubernetes holds Kubernetes-related parsed config file parameters and command-line overrides
	Kubernetes = KubernetesConfig{
		APIServer:          "http://localhost:8080",
		ServiceCIDR:        "172.16.1.0/24",
		OVNConfigNamespace: "ovn-kubernetes",
	}

	// OvnNorth holds northbound OVN database client and server authentication and location details
	OvnNorth OvnAuthConfig

	// OvnSouth holds southbound OVN database client and server authentication and location details
	OvnSouth OvnAuthConfig

	// Gateway holds node gateway-related parsed config file parameters and command-line overrides
	Gateway GatewayConfig

	// MasterHA holds master HA related config options.
	MasterHA = MasterHAConfig{
		ElectionLeaseDuration: 60,
		ElectionRenewDeadline: 30,
		ElectionRetryPeriod:   20,
	}

	// NbctlDaemon enables ovn-nbctl to run in daemon mode
	NbctlDaemonMode bool

	// UnprivilegedMode allows ovnkube-node to run without SYS_ADMIN capability, by performing interface setup in the CNI plugin
	UnprivilegedMode bool
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
	// ConntrackZone affects only the gateway nodes, This value is used to track connections
	// that are initiated from the pods so that the reverse connections go back to the pods.
	// This represents the conntrack zone used for the conntrack flow rules.
	ConntrackZone int `gcfg:"conntrack-zone"`
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
	// RawClusterSubnets holds the unparsed cluster subnets. Should only be
	// used inside config module.
	RawClusterSubnets string `gcfg:"cluster-subnets"`
	// ClusterSubnets holds parsed cluster subnet entries and may be used
	// outside the config module.
	ClusterSubnets []CIDRNetworkEntry
}

// LoggingConfig holds logging-related parsed config file parameters and command-line overrides
type LoggingConfig struct {
	// File is the path of the file to log to
	File string `gcfg:"logfile"`
	// Level is the logging verbosity level
	Level int `gcfg:"loglevel"`
}

// CNIConfig holds CNI-related parsed config file parameters and command-line overrides
type CNIConfig struct {
	// ConfDir specifies the CNI config directory in which to write the overlay CNI config file
	ConfDir string `gcfg:"conf-dir"`
	// Plugin specifies the name of the CNI plugin
	Plugin string `gcfg:"plugin"`
	// Windows ONLY, specifies the ID of the HNS Network to which the containers will be attached
	WinHNSNetworkID string `gcfg:"win-hnsnetwork-id"`
}

// KubernetesConfig holds Kubernetes-related parsed config file parameters and command-line overrides
type KubernetesConfig struct {
	Kubeconfig         string `gcfg:"kubeconfig"`
	CACert             string `gcfg:"cacert"`
	APIServer          string `gcfg:"apiserver"`
	Token              string `gcfg:"token"`
	ServiceCIDR        string `gcfg:"service-cidr"`
	OVNConfigNamespace string `gcfg:"ovn-config-namespace"`
	MetricsBindAddress string `gcfg:"metrics-bind-address"`
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
	// NextHop is the gateway IP address of Interface; will be autodetected if not given
	NextHop string `gcfg:"next-hop"`
	// VLANID is the option VLAN tag to apply to gateway traffic for "shared" mode
	VLANID uint `gcfg:"vlan-id"`
	// NodeportEnable sets whether to provide Kubernetes NodePort service or not
	NodeportEnable bool `gcfg:"nodeport"`
}

// OvnAuthConfig holds client authentication and location details for
// an OVN database (either northbound or southbound)
type OvnAuthConfig struct {
	// e.g: "ssl:192.168.1.2:6641,ssl:192.168.1.2:6642"
	Address string `gcfg:"address"`
	PrivKey string `gcfg:"client-privkey"`
	Cert    string `gcfg:"client-cert"`
	CACert  string `gcfg:"client-cacert"`
	Scheme  OvnDBScheme

	northbound bool
	externalID string // ovn-nb or ovn-remote

	exec kexec.Interface
}

// MasterHAConfig holds configuration for master HA
// configuration.
type MasterHAConfig struct {
	ElectionLeaseDuration int `gcfg:"election-lease-duration"`
	ElectionRenewDeadline int `gcfg:"election-renew-deadline"`
	ElectionRetryPeriod   int `gcfg:"election-retry-period"`
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
	Default    DefaultConfig
	Logging    LoggingConfig
	CNI        CNIConfig
	Kubernetes KubernetesConfig
	OvnNorth   OvnAuthConfig
	OvnSouth   OvnAuthConfig
	Gateway    GatewayConfig
	MasterHA   MasterHAConfig
}

var (
	savedDefault    DefaultConfig
	savedLogging    LoggingConfig
	savedCNI        CNIConfig
	savedKubernetes KubernetesConfig
	savedOvnNorth   OvnAuthConfig
	savedOvnSouth   OvnAuthConfig
	savedGateway    GatewayConfig
	savedMasterHA   MasterHAConfig
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
	// Cache original default config values so they can be restored by testcases
	savedDefault = Default
	savedLogging = Logging
	savedCNI = CNI
	savedKubernetes = Kubernetes
	savedOvnNorth = OvnNorth
	savedOvnSouth = OvnSouth
	savedGateway = Gateway
	savedMasterHA = MasterHA
	Flags = append(Flags, CommonFlags...)
	Flags = append(Flags, CNIFlags...)
	Flags = append(Flags, K8sFlags...)
	Flags = append(Flags, OvnNBFlags...)
	Flags = append(Flags, OvnSBFlags...)
	Flags = append(Flags, OVNGatewayFlags...)
	Flags = append(Flags, MasterHAFlags...)
}

// RestoreDefaultConfig restores default config values. Used by testcases to
// provide a pristine environment between tests.
func RestoreDefaultConfig() {
	Default = savedDefault
	Logging = savedLogging
	CNI = savedCNI
	Kubernetes = savedKubernetes
	OvnNorth = savedOvnNorth
	OvnSouth = savedOvnSouth
	Gateway = savedGateway
	MasterHA = savedMasterHA
}

// copy members of struct 'src' into the corresponding field in struct 'dst'
// if the field in 'src' is a non-zero int or a non-zero-length string. This
// function should be called with pointers to structs.
func overrideFields(dst, src interface{}) error {
	dstStruct := reflect.ValueOf(dst).Elem()
	srcStruct := reflect.ValueOf(src).Elem()
	if dstStruct.Kind() != srcStruct.Kind() || dstStruct.Kind() != reflect.Struct {
		return fmt.Errorf("mismatched value types")
	}
	if dstStruct.NumField() != srcStruct.NumField() {
		return fmt.Errorf("mismatched struct types")
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
		if !dstField.IsValid() || !srcField.IsValid() {
			return fmt.Errorf("invalid struct %q field %q", dstType.Name(), structField.Name)
		}
		if dstField.Kind() != srcField.Kind() {
			return fmt.Errorf("mismatched struct %q fields %q", dstType.Name(), structField.Name)
		}
		switch srcField.Kind() {
		case reflect.String:
			if srcField.String() != "" {
				dstField.Set(srcField)
			}
		case reflect.Int:
			if srcField.Int() != 0 {
				dstField.Set(srcField)
			}
		case reflect.Uint:
			if srcField.Uint() != 0 {
				dstField.Set(srcField)
			}
		case reflect.Bool:
			if srcField.Bool() {
				dstField.Set(srcField)
			}
		default:
			return fmt.Errorf("unhandled struct %q field %q type %v", dstType.Name(), structField.Name, srcField.Kind())
		}
	}
	if !handled {
		// No tags found in the struct so we don't know how to override
		return fmt.Errorf("failed to find 'gcfg' tags in struct %q", dstType.Name())
	}

	return nil
}

var cliConfig config

//CommonFlags capture general options.
var CommonFlags = []cli.Flag{
	// Mode flags
	cli.StringFlag{
		Name:  "init-master",
		Usage: "initialize master, requires the hostname as argument",
	},
	cli.StringFlag{
		Name:  "init-node",
		Usage: "initialize node, requires the name that node is registered with in kubernetes cluster",
	},
	cli.StringFlag{
		Name:  "cleanup-node",
		Usage: "cleanup node, requires the name that node is registered with in kubernetes cluster",
	},
	cli.StringFlag{
		Name:  "pidfile",
		Usage: "Name of file that will hold the ovnkube pid (optional)",
	},
	cli.StringFlag{
		Name:  "config-file",
		Usage: "configuration file path (default: /etc/openvswitch/ovn_k8s.conf)",
	},
	cli.IntFlag{
		Name:        "mtu",
		Usage:       "MTU value used for the overlay networks (default: 1400)",
		Destination: &cliConfig.Default.MTU,
	},
	cli.IntFlag{
		Name:        "conntrack-zone",
		Usage:       "For gateway nodes, the conntrack zone used for conntrack flow rules (default: 64000)",
		Destination: &cliConfig.Default.ConntrackZone,
	},
	cli.StringFlag{
		Name:        "encap-type",
		Usage:       "The encapsulation protocol to use to transmit packets between hypervisors (default: geneve)",
		Destination: &cliConfig.Default.EncapType,
	},
	cli.StringFlag{
		Name:        "encap-ip",
		Usage:       "The IP address of the encapsulation endpoint (default: Node IP address resolved from Node hostname)",
		Destination: &cliConfig.Default.EncapIP,
	},
	cli.UintFlag{
		Name:        "encap-port",
		Usage:       "The UDP port used by the encapsulation endpoint (default: 6081)",
		Destination: &cliConfig.Default.EncapPort,
	},
	cli.IntFlag{
		Name: "inactivity-probe",
		Usage: "Maximum number of milliseconds of idle time on " +
			"connection for ovn-controller before it sends a inactivity probe",
		Destination: &cliConfig.Default.InactivityProbe,
	},
	cli.StringFlag{
		Name:        "cluster-subnet",
		Usage:       "Deprecated alias for cluster-subnets.",
		Destination: &clusterSubnet,
	},
	cli.StringFlag{
		Name:  "cluster-subnets",
		Value: "10.128.0.0/14/23",
		Usage: "A comma separated set of IP subnets and the associated " +
			"hostsubnet prefix lengths to use for the cluster (eg, \"10.128.0.0/14/23,10.0.0.0/14/23\"). " +
			"Each entry is given in the form [IP address/prefix-length/hostsubnet-prefix-length] " +
			"and cannot overlap with other entries. The hostsubnet-prefix-length " +
			"is optional and if unspecified defaults to 24. The " +
			"hostsubnet-prefix-length defines how many IP addresses " +
			"are dedicated to each node and may be different for each " +
			"entry.",
		Destination: &cliConfig.Default.RawClusterSubnets,
	},
	cli.BoolFlag{
		Name:        "nbctl-daemon-mode",
		Usage:       "Run ovn-nbctl in daemon mode to improve performance in large clusters",
		Destination: &NbctlDaemonMode,
	},
	cli.BoolFlag{
		Name:        "unprivileged-mode",
		Usage:       "Run ovnkube-node container in unprivileged mode. Valid only with --init-node option.",
		Destination: &UnprivilegedMode,
	},

	// Logging options
	cli.IntFlag{
		Name:        "loglevel",
		Usage:       "log verbosity and level: 5=debug, 4=info, 3=warn, 2=error, 1=fatal (default: 4)",
		Destination: &cliConfig.Logging.Level,
	},
	cli.StringFlag{
		Name:        "logfile",
		Usage:       "path of a file to direct log output to",
		Destination: &cliConfig.Logging.File,
	},
}

// CNIFlags capture CNI-related options
var CNIFlags = []cli.Flag{
	// CNI options
	cli.StringFlag{
		Name:        "cni-conf-dir",
		Usage:       "the CNI config directory in which to write the overlay CNI config file (default: /etc/cni/net.d)",
		Destination: &cliConfig.CNI.ConfDir,
	},
	cli.StringFlag{
		Name:        "cni-plugin",
		Usage:       "the name of the CNI plugin (default: ovn-k8s-cni-overlay)",
		Destination: &cliConfig.CNI.Plugin,
	},
	cli.StringFlag{
		Name:        "win-hnsnetwork-id",
		Usage:       "the ID of the HNS network to which containers will be attached (default: not set)",
		Destination: &cliConfig.CNI.WinHNSNetworkID,
	},
}

// K8sFlags capture Kubernetes-related options
var K8sFlags = []cli.Flag{
	cli.StringFlag{
		Name:        "service-cluster-ip-range",
		Usage:       "Deprecated alias for k8s-service-cidr.",
		Destination: &serviceClusterIPRange,
	},
	cli.StringFlag{
		Name: "k8s-service-cidr",
		Usage: "A CIDR notation IP range from which k8s assigns " +
			"service cluster IPs. This should be the same as the one " +
			"provided for kube-apiserver \"-service-cluster-ip-range\" " +
			"option. (default: 172.16.1.0/24)",
		Destination: &cliConfig.Kubernetes.ServiceCIDR,
	},
	cli.StringFlag{
		Name:        "k8s-kubeconfig",
		Usage:       "absolute path to the Kubernetes kubeconfig file (not required if the --k8s-apiserver, --k8s-ca-cert, and --k8s-token are given)",
		Destination: &cliConfig.Kubernetes.Kubeconfig,
	},
	cli.StringFlag{
		Name:        "k8s-apiserver",
		Usage:       "URL of the Kubernetes API server (not required if --k8s-kubeconfig is given) (default: http://localhost:8443)",
		Destination: &cliConfig.Kubernetes.APIServer,
	},
	cli.StringFlag{
		Name:        "k8s-cacert",
		Usage:       "the absolute path to the Kubernetes API CA certificate (not required if --k8s-kubeconfig is given)",
		Destination: &cliConfig.Kubernetes.CACert,
	},
	cli.StringFlag{
		Name:        "k8s-token",
		Usage:       "the Kubernetes API authentication token (not required if --k8s-kubeconfig is given)",
		Destination: &cliConfig.Kubernetes.Token,
	},
	cli.StringFlag{
		Name:        "ovn-config-namespace",
		Usage:       "specify a namespace which will contain services to config the OVN databases",
		Destination: &cliConfig.Kubernetes.OVNConfigNamespace,
	},
	cli.StringFlag{
		Name:        "metrics-bind-address",
		Usage:       "The IP address and port for the metrics server to serve on (set to 0.0.0.0 for all IPv4 interfaces)",
		Destination: &cliConfig.Kubernetes.MetricsBindAddress,
	},
}

// OvnNBFlags capture OVN northbound database options
var OvnNBFlags = []cli.Flag{
	cli.StringFlag{
		Name: "nb-address",
		Usage: "IP address and port of the OVN northbound API " +
			"(eg, ssl://1.2.3.4:6641,ssl://1.2.3.5:6642).  Leave empty to " +
			"use a local unix socket.",
		Destination: &cliConfig.OvnNorth.Address,
	},
	cli.StringFlag{
		Name:        "nb-client-privkey",
		Usage:       "Private key that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnnb-privkey.pem)",
		Destination: &cliConfig.OvnNorth.PrivKey,
	},
	cli.StringFlag{
		Name:        "nb-client-cert",
		Usage:       "Client certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnnb-cert.pem)",
		Destination: &cliConfig.OvnNorth.Cert,
	},
	cli.StringFlag{
		Name:        "nb-client-cacert",
		Usage:       "CA certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnnb-ca.cert)",
		Destination: &cliConfig.OvnNorth.CACert,
	},
}

//OvnSBFlags capture OVN southbound database options
var OvnSBFlags = []cli.Flag{
	cli.StringFlag{
		Name: "sb-address",
		Usage: "IP address and port of the OVN southbound API " +
			"(eg, ssl://1.2.3.4:6642,ssl://1.2.3.5:6642).  " +
			"Leave empty to use a local unix socket.",
		Destination: &cliConfig.OvnSouth.Address,
	},
	cli.StringFlag{
		Name:        "sb-client-privkey",
		Usage:       "Private key that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnsb-privkey.pem)",
		Destination: &cliConfig.OvnSouth.PrivKey,
	},
	cli.StringFlag{
		Name:        "sb-client-cert",
		Usage:       "Client certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnsb-cert.pem)",
		Destination: &cliConfig.OvnSouth.Cert,
	},
	cli.StringFlag{
		Name:        "sb-client-cacert",
		Usage:       "CA certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnsb-ca.cert)",
		Destination: &cliConfig.OvnSouth.CACert,
	},
}

//OVNGatewayFlags capture L3 Gateway related flags
var OVNGatewayFlags = []cli.Flag{
	cli.StringFlag{
		Name: "gateway-mode",
		Usage: "Sets the cluster gateway mode. One of \"shared\", " +
			"or \"local\". If not given, gateway functionality is disabled.",
	},
	cli.StringFlag{
		Name: "gateway-interface",
		Usage: "The interface in minions that will be the gateway interface. " +
			"If none specified, then the node's interface on which the " +
			"default gateway is configured will be used as the gateway " +
			"interface. Only useful with \"init-gateways\"",
		Destination: &cliConfig.Gateway.Interface,
	},
	cli.StringFlag{
		Name: "gateway-nexthop",
		Usage: "The external default gateway which is used as a next hop by " +
			"OVN gateway.  This is many times just the default gateway " +
			"of the node in question. If not specified, the default gateway" +
			"configured in the node is used. Only useful with " +
			"\"init-gateways\"",
		Destination: &cliConfig.Gateway.NextHop,
	},
	cli.UintFlag{
		Name: "gateway-vlanid",
		Usage: "The VLAN on which the external network is available. " +
			"Valid only for Shared Gateway interface mode.",
		Destination: &cliConfig.Gateway.VLANID,
	},
	cli.BoolFlag{
		Name:        "nodeport",
		Usage:       "Setup nodeport based ingress on gateways.",
		Destination: &cliConfig.Gateway.NodeportEnable,
	},

	// Deprecated CLI options
	cli.BoolFlag{
		Name:        "init-gateways",
		Usage:       "DEPRECATED; use --gateway-mode instead",
		Destination: &initGateways,
	},
	cli.BoolFlag{
		Name:        "gateway-local",
		Usage:       "DEPRECATED; use --gateway-mode instead",
		Destination: &gatewayLocal,
	},
}

// MasterHAFlags capture OVN northbound database options
var MasterHAFlags = []cli.Flag{
	cli.IntFlag{
		Name:        "ha-election-lease-duration",
		Usage:       "Leader election lease duration (in secs) (default: 60)",
		Destination: &cliConfig.MasterHA.ElectionLeaseDuration,
	},
	cli.IntFlag{
		Name:        "ha-election-renew-deadline",
		Usage:       "Leader election renew deadline (in secs) (default: 35)",
		Destination: &cliConfig.MasterHA.ElectionRenewDeadline,
	},
	cli.IntFlag{
		Name:        "ha-election-retry-period",
		Usage:       "Leader election retry period (in secs) (default: 10)",
		Destination: &cliConfig.MasterHA.ElectionRetryPeriod,
	},
}

// Flags are general command-line flags. Apps should add these flags to their
// own urfave/cli flags and call InitConfig() early in the application.
var Flags []cli.Flag

// Defaults are a set of flags to indicate which options should be read from
// ovs-vsctl and used as default values if option is not found via the config
// file or command-line
type Defaults struct {
	OvnNorthAddress bool
	K8sAPIServer    bool
	K8sToken        bool
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

	logrus.Debugf("exec: %s %s", cmdPath, strings.Join(args, " "))
	out, err := exec.Command(cmdPath, args...).CombinedOutput()
	if err != nil {
		logrus.Debugf("exec: %s %s => %v", cmdPath, strings.Join(args, " "), err)
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
		logrus.Debugf("failed to get OVS external_id %s: %v\n\t%s", name, err, out)
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
		return fmt.Errorf("Error setting OVS external ID '%s=%s': %v\n  %q", key, value, err, out)
	}
	return nil
}

func buildKubernetesConfig(exec kexec.Interface, cli, file *config, saPath string, defaults *Defaults) error {
	// token adn ca.crt may be from files mounted in container.
	var saConfig KubernetesConfig
	if data, err := ioutil.ReadFile(filepath.Join(saPath, kubeServiceAccountFileToken)); err == nil {
		saConfig.Token = string(data)
	}
	if _, err2 := os.Stat(filepath.Join(saPath, kubeServiceAccountFileCACert)); err2 == nil {
		saConfig.CACert = filepath.Join(saPath, kubeServiceAccountFileCACert)
	}

	if err := overrideFields(&Kubernetes, &saConfig); err != nil {
		return err
	}

	// Grab default values from OVS external IDs
	if defaults.K8sAPIServer {
		Kubernetes.APIServer = getOVSExternalID(exec, "k8s-api-server")
	}
	if defaults.K8sToken {
		Kubernetes.Token = getOVSExternalID(exec, "k8s-api-token")
	}
	if defaults.K8sCert {
		Kubernetes.CACert = getOVSExternalID(exec, "k8s-ca-certificate")
	}

	// values for token, cacert, kubeconfig, api-server may be found in several places.
	// Take the first found when looking in this order: command line options, config file,
	// environment variables, service account files

	envConfig := KubernetesConfig{
		Kubeconfig: os.Getenv("KUBECONFIG"),
		CACert:     os.Getenv("K8S_CACERT"),
		APIServer:  os.Getenv("K8S_APISERVER"),
		Token:      os.Getenv("K8S_TOKEN"),
	}

	if err := overrideFields(&Kubernetes, &envConfig); err != nil {
		return err
	}

	// Copy config file values over default values
	if err := overrideFields(&Kubernetes, &file.Kubernetes); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&Kubernetes, &cli.Kubernetes); err != nil {
		return err
	}

	if Kubernetes.Kubeconfig != "" && !pathExists(Kubernetes.Kubeconfig) {
		return fmt.Errorf("kubernetes kubeconfig file %q not found", Kubernetes.Kubeconfig)
	}
	if Kubernetes.CACert != "" && !pathExists(Kubernetes.CACert) {
		return fmt.Errorf("kubernetes CA certificate file %q not found", Kubernetes.CACert)
	}

	url, err := url.Parse(Kubernetes.APIServer)
	if err != nil {
		return fmt.Errorf("kubernetes API server address %q invalid: %v", Kubernetes.APIServer, err)
	} else if url.Scheme != "https" && url.Scheme != "http" {
		return fmt.Errorf("kubernetes API server URL scheme %q invalid", url.Scheme)
	}

	// Legacy service-cluster-ip-range CLI option overrides config file or --k8s-service-cidr
	if serviceClusterIPRange != "" {
		Kubernetes.ServiceCIDR = serviceClusterIPRange
	}
	if Kubernetes.ServiceCIDR == "" {
		return fmt.Errorf("kubernetes service-cidr is required")
	} else if _, _, err := net.ParseCIDR(Kubernetes.ServiceCIDR); err != nil {
		return fmt.Errorf("kubernetes service network CIDR %q invalid: %v", Kubernetes.ServiceCIDR, err)
	}

	return nil
}

func buildGatewayConfig(ctx *cli.Context, cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&Gateway, &file.Gateway); err != nil {
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
	if err := overrideFields(&Gateway, &cli.Gateway); err != nil {
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
		if Gateway.VLANID != 0 {
			return fmt.Errorf("gateway VLAN ID option '%d' not allowed when gateway is disabled", Gateway.VLANID)
		}
	}
	return nil
}

func buildMasterHAConfig(ctx *cli.Context, cli, file *config) error {
	// Copy config file values over default values
	if err := overrideFields(&MasterHA, &file.MasterHA); err != nil {
		return err
	}

	// And CLI overrides over config file and default values
	if err := overrideFields(&MasterHA, &cli.MasterHA); err != nil {
		return err
	}

	if MasterHA.ElectionLeaseDuration <= MasterHA.ElectionRenewDeadline {
		return fmt.Errorf("Invalid HA election lease duration '%d'. "+
			"It should be greater than HA election renew deadline '%d'",
			MasterHA.ElectionLeaseDuration, MasterHA.ElectionRenewDeadline)
	}

	if MasterHA.ElectionRenewDeadline <= MasterHA.ElectionRetryPeriod {
		return fmt.Errorf("Invalid HA election renew deadline duration '%d'. "+
			"It should be greater than HA election retry period '%d'",
			MasterHA.ElectionRenewDeadline, MasterHA.ElectionRetryPeriod)
	}
	return nil
}

func buildDefaultConfig(cli, file *config) error {
	if err := overrideFields(&Default, &file.Default); err != nil {
		return err
	}

	if err := overrideFields(&Default, &cli.Default); err != nil {
		return err
	}

	// Legacy cluster-subnet CLI option overrides config file or --cluster-subnets
	if clusterSubnet != "" {
		Default.RawClusterSubnets = clusterSubnet
	}
	if Default.RawClusterSubnets == "" {
		return fmt.Errorf("cluster subnet is required")
	}

	var err error
	Default.ClusterSubnets, err = ParseClusterSubnetEntries(Default.RawClusterSubnets)
	if err != nil {
		return fmt.Errorf("cluster subnet invalid: %v", err)
	}
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

	// Linux default
	if runtime.GOOS != "windows" {
		return filepath.Join("/etc", "openvswitch", "ovn_k8s.conf"), true
	}

	// Windows default
	return filepath.Join(os.Getenv("OVS_SYSCONFDIR"), "ovn_k8s.conf"), true
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

// initConfigWithPath reads the given config file (or if empty, reads the config file
// specified by command-line arguments, or empty, the default config file) and
// common command-line options and constructs the global config object from
// them. It returns the config file path (if explicitly specified) or an error
func initConfigWithPath(ctx *cli.Context, exec kexec.Interface, saPath string, defaults *Defaults) (string, error) {
	var cfg config
	var retConfigFile string
	var configFile string
	var configFileIsDefault bool
	var err error

	configFile, configFileIsDefault = getConfigFilePath(ctx)

	logrus.SetOutput(os.Stderr)

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
			return "", fmt.Errorf("failed to parse config file %s: %v", f.Name(), err)
		}
		logrus.Infof("Parsed config file %s", f.Name())
		logrus.Infof("Parsed config: %+v", cfg)
	}

	if defaults == nil {
		defaults = &Defaults{}
	}

	// Build config that needs no special processing
	if err = overrideFields(&CNI, &cfg.CNI); err != nil {
		return "", err
	}
	if err = overrideFields(&CNI, &cliConfig.CNI); err != nil {
		return "", err
	}

	// Logging setup
	if err = overrideFields(&Logging, &cfg.Logging); err != nil {
		return "", err
	}
	if err = overrideFields(&Logging, &cliConfig.Logging); err != nil {
		return "", err
	}

	logrus.SetLevel(logrus.Level(Logging.Level))
	if Logging.File != "" {
		var file *os.File
		if _, err = os.Stat(filepath.Dir(Logging.File)); os.IsNotExist(err) {
			dir := filepath.Dir(Logging.File)
			if err = os.MkdirAll(dir, 0755); err != nil {
				logrus.Errorf("failed to create logfile directory %s (%v). Ignoring..", dir, err)
			}
		}
		file, err = os.OpenFile(Logging.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0660)
		if err != nil {
			logrus.Errorf("failed to open logfile %s (%v). Ignoring..", Logging.File, err)
		} else {
			logrus.SetOutput(file)
		}
	}

	if err = buildDefaultConfig(&cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildKubernetesConfig(exec, &cliConfig, &cfg, saPath, defaults); err != nil {
		return "", err
	}

	if err = buildGatewayConfig(ctx, &cliConfig, &cfg); err != nil {
		return "", err
	}

	if err = buildMasterHAConfig(ctx, &cliConfig, &cfg); err != nil {
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

	logrus.Debugf("Default config: %+v", Default)
	logrus.Debugf("Logging config: %+v", Logging)
	logrus.Debugf("CNI config: %+v", CNI)
	logrus.Debugf("Kubernetes config: %+v", Kubernetes)
	logrus.Debugf("OVN North config: %+v", OvnNorth)
	logrus.Debugf("OVN South config: %+v", OvnSouth)

	return retConfigFile, nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	if err != nil && os.IsNotExist(err) {
		return false
	}
	return true
}

// parseAddress parses an OVN database address, which can be of form
// "ssl:1.2.3.4:6641,ssl:1.2.3.5:6641" or "ssl://1.2.3.4:6641,ssl://1.2.3.5:6641"
// and returns the validated address(es) and the scheme
func parseAddress(urlString string) (string, OvnDBScheme, error) {
	var parsedAddress, scheme string
	var parsedScheme OvnDBScheme

	urlString = strings.Replace(urlString, "//", "", -1)
	for _, ovnAddress := range strings.Split(urlString, ",") {
		splits := strings.Split(ovnAddress, ":")
		if len(splits) != 3 {
			return "", "", fmt.Errorf("Failed to parse OVN address %s", urlString)
		}
		hostPort := splits[1] + ":" + splits[2]

		if scheme == "" {
			scheme = splits[0]
		} else if scheme != splits[0] {
			return "", "", fmt.Errorf("Invalid protocols in OVN address %s",
				urlString)
		}

		host, port, err := net.SplitHostPort(hostPort)
		if err != nil {
			return "", "", fmt.Errorf("failed to parse OVN DB host/port %q: %v",
				hostPort, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return "", "", fmt.Errorf("OVN DB host %q must be an IP address, "+
				"not a DNS name", hostPort)
		}

		if parsedAddress != "" {
			parsedAddress += ","
		}
		parsedAddress += fmt.Sprintf("%s:%s:%s", scheme, host, port)
	}

	switch {
	case scheme == "ssl":
		parsedScheme = OvnDBSchemeSSL
	case scheme == "tcp":
		parsedScheme = OvnDBSchemeTCP
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
	if northbound {
		auth.externalID = "ovn-nb"
		direction = "nb"
	} else {
		auth.externalID = "ovn-remote"
		direction = "sb"
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
	if err := overrideFields(auth, confAuth); err != nil {
		return nil, err
	}
	if err := overrideFields(auth, cliAuth); err != nil {
		return nil, err
	}

	if address == "" {
		if auth.PrivKey != "" || auth.Cert != "" || auth.CACert != "" {
			return nil, fmt.Errorf("certificate or key given; perhaps you mean to use the 'ssl' scheme?")
		}
		auth.Scheme = OvnDBSchemeUnix
		return auth, nil
	}

	var err error
	auth.Address, auth.Scheme, err = parseAddress(address)
	if err != nil {
		return nil, err
	}

	switch {
	case auth.Scheme == OvnDBSchemeSSL:
		if auth.PrivKey == "" || auth.Cert == "" || auth.CACert == "" {
			return nil, fmt.Errorf("must specify private key, certificate, and CA certificate for 'ssl' scheme")
		}
	case auth.Scheme == OvnDBSchemeTCP:
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
		logrus.Warnf("bootstrapping %s CA certificate failed", a.CACert)
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
	if a.Scheme == OvnDBSchemeUnix {
		// Nothing to do
		return nil
	} else if a.Scheme == OvnDBSchemeSSL {
		// Both server and client SSL schemes require privkey and cert
		if !pathExists(a.PrivKey) {
			return fmt.Errorf("private key file %s not found", a.PrivKey)
		}
		if !pathExists(a.Cert) {
			return fmt.Errorf("certificate file %s not found", a.Cert)
		}
	}

	if a.Scheme == OvnDBSchemeSSL {
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

	if err := setOVSExternalID(a.exec, a.externalID, "\""+a.GetURL()+"\""); err != nil {
		return err
	}
	return nil
}

func (a *OvnAuthConfig) updateIP(newIP []string, port string) error {
	if a.Address != "" {
		s := strings.Split(a.Address, ":")
		if len(s) != 3 {
			return fmt.Errorf("failed to parse OvnAuthConfig address %q", a.Address)
		}
		var newPort string
		if port != "" {
			newPort = port
		} else {
			newPort = s[2]
		}

		newAddresses := make([]string, 0, len(newIP))
		for _, ipAddress := range newIP {
			newAddresses = append(newAddresses, s[0]+":"+ipAddress+":"+newPort)
		}
		a.Address = strings.Join(newAddresses, ",")
	}
	return nil
}

// UpdateOVNNodeAuth updates the host and URL in ClientAuth
// for both OvnNorth and OvnSouth. It updates them with the new masterIP.
func UpdateOVNNodeAuth(masterIP []string, southboundDBPort, northboundDBPort string) error {
	logrus.Debugf("Update OVN node auth with new master ip: %s", masterIP)
	if err := OvnNorth.updateIP(masterIP, northboundDBPort); err != nil {
		return fmt.Errorf("failed to update OvnNorth ClientAuth URL: %v", err)
	}

	if err := OvnSouth.updateIP(masterIP, southboundDBPort); err != nil {
		return fmt.Errorf("failed to update OvnSouth ClientAuth URL: %v", err)
	}
	return nil
}
