package config

import (
	"fmt"
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

// The following are global config parameters that other modules may access directly
var (
	// ovn-kubernetes version, to be changed with every release
	Version = "0.3.0"

	// Default holds parsed config file parameters and command-line overrides
	Default = DefaultConfig{
		MTU:           1400,
		ConntrackZone: 64000,
		EncapType:     "geneve",
		EncapIP:       "",
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
		APIServer: "http://localhost:8080",
	}

	// OvnNorth holds northbound OVN database client and server authentication and location details
	OvnNorth OvnAuthConfig

	// OvnSouth holds southbound OVN database client and server authentication and location details
	OvnSouth OvnAuthConfig
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
	Kubeconfig string `gcfg:"kubeconfig"`
	CACert     string `gcfg:"cacert"`
	APIServer  string `gcfg:"apiserver"`
	Token      string `gcfg:"token"`
}

// OvnAuthConfig holds client and server authentication and location details for
// an OVN database (either northbound or southbound)
type OvnAuthConfig struct {
	ClientAuth *OvnDBAuth
	ServerAuth *OvnDBAuth
}

// Holds values read from the config file or command-line that are then
// synthesized into OvnDBAuth structures in an OvnAuthConfig object
type rawOvnAuthConfig struct {
	Address       string `gcfg:"address"`
	ClientPrivKey string `gcfg:"client-privkey"`
	ClientCert    string `gcfg:"client-cert"`
	ClientCACert  string `gcfg:"client-cacert"`
	ServerPrivKey string `gcfg:"server-privkey"`
	ServerCert    string `gcfg:"server-cert"`
	ServerCACert  string `gcfg:"server-cacert"`
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
	OvnNorth   rawOvnAuthConfig
	OvnSouth   rawOvnAuthConfig
}

var (
	savedDefault    DefaultConfig
	savedLogging    LoggingConfig
	savedCNI        CNIConfig
	savedKubernetes KubernetesConfig
	savedOvnNorth   OvnAuthConfig
	savedOvnSouth   OvnAuthConfig
)

func init() {
	// Cache original default config values so they can be restored by testcases
	savedDefault = Default
	savedLogging = Logging
	savedCNI = CNI
	savedKubernetes = Kubernetes
	savedOvnNorth = OvnNorth
	savedOvnSouth = OvnSouth
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
}

// copy members of struct 'src' into the corresponding field in struct 'dst'
// if the field in 'src' is a non-zero int or a non-zero-length string. This
// function should be called with pointers to structs.
func overrideFields(dst, src interface{}) {
	dstStruct := reflect.ValueOf(dst).Elem()
	srcStruct := reflect.ValueOf(src).Elem()
	if dstStruct.Kind() != srcStruct.Kind() || dstStruct.Kind() != reflect.Struct {
		panic("mismatched value types")
	}
	if dstStruct.NumField() != srcStruct.NumField() {
		panic("mismatched struct types")
	}

	for i := 0; i < dstStruct.NumField(); i++ {
		dstField := dstStruct.Field(i)
		srcField := srcStruct.Field(i)
		if dstField.Kind() != srcField.Kind() {
			panic("mismatched struct fields")
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
		default:
			panic(fmt.Sprintf("unhandled struct field type: %v", srcField.Kind()))
		}
	}
}

var cliConfig config

// Flags are general command-line flags. Apps should add these flags to their
// own urfave/cli flags and call InitConfig() early in the application.
var Flags = []cli.Flag{
	cli.StringFlag{
		Name:  "config-file",
		Usage: "configuration file path (default: /etc/openvswitch/ovn_k8s.conf)",
	},

	// Generic options
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

	// Kubernetes-related options
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

	// OVN northbound database options
	cli.StringFlag{
		Name:        "nb-address",
		Usage:       "IP address and port of the OVN northbound API (eg, ssl://1.2.3.4:6641).  Leave empty to use a local unix socket.",
		Destination: &cliConfig.OvnNorth.Address,
	},
	cli.StringFlag{
		Name:        "nb-server-privkey",
		Usage:       "Private key that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnnb-privkey.pem)",
		Destination: &cliConfig.OvnNorth.ServerPrivKey,
	},
	cli.StringFlag{
		Name:        "nb-server-cert",
		Usage:       "Server certificate that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnnb-cert.pem)",
		Destination: &cliConfig.OvnNorth.ServerCert,
	},
	cli.StringFlag{
		Name:        "nb-server-cacert",
		Usage:       "CA certificate that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnnb-ca.cert)",
		Destination: &cliConfig.OvnNorth.ServerCACert,
	},
	cli.StringFlag{
		Name:        "nb-client-privkey",
		Usage:       "Private key that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnnb-privkey.pem)",
		Destination: &cliConfig.OvnNorth.ClientPrivKey,
	},
	cli.StringFlag{
		Name:        "nb-client-cert",
		Usage:       "Client certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnnb-cert.pem)",
		Destination: &cliConfig.OvnNorth.ClientCert,
	},
	cli.StringFlag{
		Name:        "nb-client-cacert",
		Usage:       "CA certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnnb-ca.cert)",
		Destination: &cliConfig.OvnNorth.ClientCACert,
	},

	// OVN southbound database options
	cli.StringFlag{
		Name:        "sb-address",
		Usage:       "IP address and port of the OVN southbound API (eg, ssl://1.2.3.4:6642).  Leave empty to use a local unix socket.",
		Destination: &cliConfig.OvnSouth.Address,
	},
	cli.StringFlag{
		Name:        "sb-server-privkey",
		Usage:       "Private key that the OVN southbound API should use for securing the API.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnsb-privkey.pem)",
		Destination: &cliConfig.OvnSouth.ServerPrivKey,
	},
	cli.StringFlag{
		Name:        "sb-server-cert",
		Usage:       "Server certificate that the OVN southbound API should use for securing the API.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnsb-cert.pem)",
		Destination: &cliConfig.OvnSouth.ServerCert,
	},
	cli.StringFlag{
		Name:        "sb-server-cacert",
		Usage:       "CA certificate that the OVN southbound API should use for securing the API.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnsb-ca.cert)",
		Destination: &cliConfig.OvnSouth.ServerCACert,
	},
	cli.StringFlag{
		Name:        "sb-client-privkey",
		Usage:       "Private key that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnsb-privkey.pem)",
		Destination: &cliConfig.OvnSouth.ClientPrivKey,
	},
	cli.StringFlag{
		Name:        "sb-client-cert",
		Usage:       "Client certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnsb-cert.pem)",
		Destination: &cliConfig.OvnSouth.ClientCert,
	},
	cli.StringFlag{
		Name:        "sb-client-cacert",
		Usage:       "CA certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket. (default: /etc/openvswitch/ovnsb-ca.cert)",
		Destination: &cliConfig.OvnSouth.ClientCACert,
	},
}

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
	newArgs := append([]string{"--timeout=5"}, args...)
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

func buildKubernetesConfig(exec kexec.Interface, cli, file *config, defaults *Defaults) error {
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

	// Copy config file values over default values
	overrideFields(&Kubernetes, &file.Kubernetes)
	// And CLI overrides over config file and default values
	overrideFields(&Kubernetes, &cli.Kubernetes)

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

	return nil
}

func buildOvnAuth(exec kexec.Interface, direction, externalID string, cliAuth, confAuth *rawOvnAuthConfig, readAddress bool) (OvnAuthConfig, error) {
	ctlCmd := "ovn-" + direction + "ctl"

	// Determine final address so we know how to set cert/key defaults
	address := cliAuth.Address
	if address == "" {
		address = confAuth.Address
	}
	if address == "" && readAddress {
		address = getOVSExternalID(exec, "ovn-"+direction)
		// address will be in format ssl:1.2.3.4:6641 from external_ids,
		// but we want it in url format, i.e. ssl://1.2.3.4:6641
		address = strings.Replace(address, ":", "://", 1)
	}

	auth := &rawOvnAuthConfig{Address: address}
	if strings.HasPrefix(address, "ssl") {
		// Set up default SSL cert/key paths
		auth.ClientCACert = "/etc/openvswitch/ovn" + direction + "-ca.cert"
		auth.ServerCACert = auth.ClientCACert
		auth.ClientPrivKey = "/etc/openvswitch/ovn" + direction + "-privkey.pem"
		auth.ServerPrivKey = auth.ClientPrivKey
		auth.ClientCert = "/etc/openvswitch/ovn" + direction + "-cert.pem"
		auth.ServerCert = auth.ClientCert
	}
	overrideFields(auth, confAuth)
	overrideFields(auth, cliAuth)

	clientAuth, err := newOvnDBAuth(exec, ctlCmd, externalID, auth.Address, auth.ClientPrivKey, auth.ClientCert, auth.ClientCACert, false)
	if err != nil {
		return OvnAuthConfig{}, err
	}
	serverAuth, err := newOvnDBAuth(exec, ctlCmd, externalID, auth.Address, auth.ServerPrivKey, auth.ServerCert, auth.ServerCACert, true)
	if err != nil {
		return OvnAuthConfig{}, err
	}

	return OvnAuthConfig{
		ClientAuth: clientAuth,
		ServerAuth: serverAuth,
	}, nil
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
	return InitConfigWithPath(ctx, exec, "", defaults)
}

// InitConfigWithPath reads the given config file (or if empty, reads the config file
// specified by command-line arguments, or empty, the default config file) and
// common command-line options and constructs the global config object from
// them. It returns the config file path (if explicitly specified) or an error
func InitConfigWithPath(ctx *cli.Context, exec kexec.Interface, configFile string, defaults *Defaults) (string, error) {
	var cfg config
	var retConfigFile string
	var configFileIsDefault bool

	// If no specific config file was given, try to find one from command-line
	// arguments, or the platform-specific default config file path
	if configFile == "" {
		configFile, configFileIsDefault = getConfigFilePath(ctx)
	}

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
	overrideFields(&Default, &cfg.Default)
	overrideFields(&Default, &cliConfig.Default)
	overrideFields(&CNI, &cfg.CNI)
	overrideFields(&CNI, &cliConfig.CNI)

	// Logging setup
	overrideFields(&Logging, &cfg.Logging)
	overrideFields(&Logging, &cliConfig.Logging)
	logrus.SetLevel(logrus.Level(Logging.Level))
	if Logging.File != "" {
		var file *os.File
		file, err = os.OpenFile(Logging.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0660)
		if err != nil {
			logrus.Errorf("failed to open logfile %s (%v). Ignoring..", Logging.File, err)
		} else {
			logrus.SetOutput(file)
		}
	}

	if err = buildKubernetesConfig(exec, &cliConfig, &cfg, defaults); err != nil {
		return "", err
	}

	OvnNorth, err = buildOvnAuth(exec, "nb", "ovn-nb", &cliConfig.OvnNorth, &cfg.OvnNorth, defaults.OvnNorthAddress)
	if err != nil {
		return "", err
	}

	OvnSouth, err = buildOvnAuth(exec, "sb", "ovn-remote", &cliConfig.OvnSouth, &cfg.OvnSouth, false)
	if err != nil {
		return "", err
	}
	logrus.Debugf("Default config: %+v", Default)
	logrus.Debugf("Logging config: %+v", Logging)
	logrus.Debugf("CNI config: %+v", CNI)
	logrus.Debugf("Kubernetes config: %+v", Kubernetes)
	logrus.Debugf("OVN North config: %+v", OvnNorth)
	logrus.Debugf("OVN South config: %+v", OvnSouth)

	return retConfigFile, nil
}

// OvnDBAuth describes an OVN database location and authentication method
type OvnDBAuth struct {
	URL     string
	PrivKey string
	Cert    string
	CACert  string
	Scheme  OvnDBScheme

	server     bool
	host       string
	port       string
	ctlCmd     string
	externalID string

	exec kexec.Interface
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	if err != nil && os.IsNotExist(err) {
		return false
	}
	return true
}

// newOvnDBAuth returns an OvnDBAuth object describing the connection to an
// OVN database, given a connection description string and authentication
// details
func newOvnDBAuth(exec kexec.Interface, ctlCmd, externalID, urlString, privkey, cert, cacert string, server bool) (*OvnDBAuth, error) {
	if urlString == "" {
		if privkey != "" || cert != "" || cacert != "" {
			return nil, fmt.Errorf("certificate or key given; perhaps you mean to use the 'ssl' scheme?")
		}
		return &OvnDBAuth{
			server:     server,
			Scheme:     OvnDBSchemeUnix,
			ctlCmd:     ctlCmd,
			externalID: externalID,
			exec:       exec,
		}, nil
	}

	url, err := url.Parse(urlString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OVN DB URL %q: %v", urlString, err)
	}
	host, port, err := net.SplitHostPort(url.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OVN DB host/port %q: %v", url.Host, err)
	}
	// OVN requires the --db argument to be an IP, not a DNS name
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("OVN DB host %q must be an IP address, not a DNS name", url.Host)
	}

	auth := &OvnDBAuth{
		URL:        urlString,
		server:     server,
		host:       host,
		port:       port,
		ctlCmd:     ctlCmd,
		externalID: externalID,
		exec:       exec,
	}

	switch {
	case url.Scheme == "ssl":
		if privkey == "" || cert == "" || cacert == "" {
			return nil, fmt.Errorf("must specify private key, certificate, and CA certificate for 'ssl' scheme")
		}
		auth.Scheme = OvnDBSchemeSSL
		auth.PrivKey = privkey
		auth.Cert = cert
		auth.CACert = cacert
	case url.Scheme == "tcp":
		if privkey != "" || cert != "" || cacert != "" {
			return nil, fmt.Errorf("certificate or key given; perhaps you mean to use the 'ssl' scheme?")
		}
		auth.Scheme = OvnDBSchemeTCP
	default:
		return nil, fmt.Errorf("unknown OVN DB scheme %q", url.Scheme)
	}

	return auth, nil
}

func (a *OvnDBAuth) ensureCACert() error {
	if pathExists(a.CACert) {
		// CA file exists, nothing to do
		return nil
	}

	if a.server {
		// Server requires CA certificate file
		return fmt.Errorf("CA certificate file %s not found", a.CACert)
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
func (a *OvnDBAuth) GetURL() string {
	// FIXME: support specific IP Addresses or non-default Unix socket paths
	if a.server {
		return fmt.Sprintf("p%s:%s", a.Scheme, a.port)
	}
	return fmt.Sprintf("%s:%s:%s", a.Scheme, a.host, a.port)
}

// SetDBAuth sets the authentication configuration and connection method
// for the OVN northbound or southbound database server or client
func (a *OvnDBAuth) SetDBAuth() error {
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

	if a.server {
		// Set the database connection method
		out, err := rawExec(a.exec, a.ctlCmd, "set-connection", a.GetURL())
		if err != nil {
			return fmt.Errorf("error setting %s API connection: %v\n  %q", a.ctlCmd, err, out)
		}

		if a.Scheme == OvnDBSchemeSSL {
			// Server auth requires CA certificate to exist
			if !pathExists(a.CACert) {
				return fmt.Errorf("server CA certificate file %s not found", a.CACert)
			}
			// Tell the database what SSL keys and certs to use, but before that delete
			// any SSL configuration. Otherwise, ovn-{nbctl|sbctl} set-ssl command will hang
			out, err = rawExec(a.exec, a.ctlCmd, "del-ssl")
			if err != nil {
				return fmt.Errorf("error deleting %s SSL configuration: %v\n %q", a.ctlCmd, err, out)
			}
			out, err = rawExec(a.exec, a.ctlCmd, "set-ssl", a.PrivKey, a.Cert, a.CACert)
			if err != nil {
				return fmt.Errorf("error setting %s SSL API certificates: %v\n  %q", a.ctlCmd, err, out)
			}
		}
	} else {
		if a.Scheme == OvnDBSchemeSSL {
			// Client can bootstrap the CA cert from the DB
			if err := a.ensureCACert(); err != nil {
				return err
			}

			// Tell Southbound DB clients (like ovn-controller)
			// which certificates to use to talk to the DB.
			// Must happen *before* setting the "ovn-remote"
			// external-id.
			if a.ctlCmd == "ovn-sbctl" {
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
	}
	return nil
}

func (a *OvnDBAuth) updateIP(newIP string) error {
	a.host = newIP
	if a.URL != "" {
		s := strings.Split(a.URL, ":")
		if len(s) != 3 {
			return fmt.Errorf("failed to parse OvnDBAuth URL: %q", a.URL)
		}
		a.URL = s[0] + ":" + newIP + ":" + s[2]
	}
	return nil
}

// UpdateOvnNodeAuth updates the host and URL in ClientAuth and ServerAuth
// for both OvnNorth and OvnSouth. It updates them with the new masterIP.
func UpdateOvnNodeAuth(masterIP string) error {
	logrus.Debugf("Update OVN node auth with new master ip: %s", masterIP)
	if err := OvnNorth.ClientAuth.updateIP(masterIP); err != nil {
		return fmt.Errorf("failed to update OvnNorth ClientAuth URL: %v", err)
	}
	if err := OvnNorth.ServerAuth.updateIP(masterIP); err != nil {
		return fmt.Errorf("failed to update OvnNorth ServerAuth URL: %v", err)
	}

	if err := OvnSouth.ClientAuth.updateIP(masterIP); err != nil {
		return fmt.Errorf("failed to update OvnSouth ClientAuth URL: %v", err)
	}
	if err := OvnSouth.ServerAuth.updateIP(masterIP); err != nil {
		return fmt.Errorf("failed to update OvnSouth ServerAuth URL: %v", err)
	}
	return nil
}
