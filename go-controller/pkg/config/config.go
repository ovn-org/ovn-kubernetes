package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	gcfg "gopkg.in/gcfg.v1"
)

// The following are global config parameters that other modules may access directly
var (
	// Default holds parsed config file parameters and command-line overrides
	Default DefaultConfig
	// Logging holds logging-related parsed config file parameters and command-line overrides
	Logging LoggingConfig
	// CNI holds CNI-related parsed config file parameters and command-line overrides
	CNI CNIConfig
	// Kubernetes holds Kubernetes-related parsed config file parameters and command-line overrides
	Kubernetes KubernetesConfig
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
	// The following config values are used for the CNI plugin.
	ConfDir string `gcfg:"conf-dir"`
	Plugin  string `gcfg:"plugin"`
}

// KubernetesConfig holds Kubernetes-related parsed config file parameters and command-line overrides
type KubernetesConfig struct {
	// K8S apiserver and authentication details are stored in the following options.
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

// config is used to read the structured config file
type config struct {
	Default    DefaultConfig
	Logging    LoggingConfig
	CNI        CNIConfig
	Kubernetes KubernetesConfig
	OvnNorth   rawOvnAuthConfig
	OvnSouth   rawOvnAuthConfig
}

func getConfigInt(cmdline int, configfile int, defval int) int {
	if cmdline != 0 {
		return cmdline
	}
	if configfile != 0 {
		return configfile
	}
	return defval
}

func getConfigStr(cmdline string, configfile string, defval string) string {
	if cmdline != "" {
		return cmdline
	}
	if configfile != "" {
		return configfile
	}
	return defval
}

func getOvnAuth(north bool, cliAuth, confAuth *rawOvnAuthConfig) (*OvnDBAuth, *OvnDBAuth, error) {
	direction := "nb"
	if !north {
		direction = "sb"
	}
	ctlCmd := "ovn-" + direction + "ctl"

	address := getConfigStr(cliAuth.Address, confAuth.Address, "")

	var defaultCACert, defaultPrivKey, defaultCert string
	if strings.HasPrefix(address, "https:") {
		defaultCACert = "/etc/openvswitch/ovn" + direction + "-ca.cert"
		defaultPrivKey = "/etc/openvswitch/ovncontroller-privkey.pem"
		defaultCert = "/etc/openvswitch/ovncontroller-cert.pem"
	}

	clientPrivKey := getConfigStr(cliAuth.ClientPrivKey, confAuth.ClientPrivKey, defaultPrivKey)
	clientCert := getConfigStr(cliAuth.ClientCert, confAuth.ClientCert, defaultCert)
	clientCACert := getConfigStr(cliAuth.ClientCACert, confAuth.ClientCACert, defaultCACert)
	serverPrivKey := getConfigStr(cliAuth.ServerPrivKey, confAuth.ServerPrivKey, defaultPrivKey)
	serverCert := getConfigStr(cliAuth.ServerCert, confAuth.ServerCert, defaultCert)
	serverCACert := getConfigStr(cliAuth.ServerCACert, confAuth.ServerCACert, defaultCACert)

	clientAuth, err := newOvnDBAuth(ctlCmd, address, clientPrivKey, clientCert, clientCACert, false)
	if err != nil {
		return nil, nil, err
	}
	serverAuth, err := newOvnDBAuth(ctlCmd, address, serverPrivKey, serverCert, serverCACert, true)
	if err != nil {
		return nil, nil, err
	}

	return clientAuth, serverAuth, nil
}

var cliConfig config

// Flags are general command-line flags. Apps should add these flags to their
// own urfave/cli flags and call InitConfig() early in the application.
var Flags = []cli.Flag{
	cli.StringFlag{
		Name:  "config-file",
		Usage: "configuration file path",
	},

	// Generic options
	cli.IntFlag{
		Name:        "mtu",
		Usage:       "MTU value used for the overlay networks.",
		Destination: &cliConfig.Default.MTU,
	},
	cli.IntFlag{
		Name:        "conntrack-zone",
		Usage:       "For gateway nodes, the conntrack zone used for conntrack flow rules",
		Destination: &cliConfig.Default.ConntrackZone,
	},

	// Logging options
	cli.IntFlag{
		Name:        "loglevel",
		Usage:       "log verbosity and level: 5=debug, 4=info, 3=warn, 2=error, 1=fatal",
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
		Usage:       "the CNI config directory in which to write the overlay CNI config file",
		Destination: &cliConfig.CNI.ConfDir,
	},
	cli.StringFlag{
		Name:        "cni-plugin",
		Usage:       "the name of the CNI plugin",
		Destination: &cliConfig.CNI.Plugin,
	},

	// Kubernetes-related options
	cli.StringFlag{
		Name:        "k8s-kubeconfig",
		Usage:       "absolute path to the Kubernetes kubeconfig file (not required if the --k8s-apiserver, --k8s-ca-cert, and --k8s-token are given)",
		Destination: &cliConfig.Kubernetes.Kubeconfig,
	},
	cli.StringFlag{
		Name:        "k8s-apiserver",
		Usage:       "URL of the Kubernetes API server (not required if --k8s-kubeconfig is given)",
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
		Usage:       "Private key that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnNorth.ServerPrivKey,
	},
	cli.StringFlag{
		Name:        "nb-server-cert",
		Usage:       "Server certificate that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnNorth.ServerCert,
	},
	cli.StringFlag{
		Name:        "nb-server-cacert",
		Usage:       "CA certificate that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnNorth.ServerCACert,
	},
	cli.StringFlag{
		Name:        "nb-client-privkey",
		Usage:       "Private key that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnNorth.ClientPrivKey,
	},
	cli.StringFlag{
		Name:        "nb-client-cert",
		Usage:       "Client certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnNorth.ClientCert,
	},
	cli.StringFlag{
		Name:        "nb-client-cacert",
		Usage:       "CA certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
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
		Usage:       "Private key that the OVN southbound API should use for securing the API.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnSouth.ServerPrivKey,
	},
	cli.StringFlag{
		Name:        "sb-server-cert",
		Usage:       "Server certificate that the OVN southbound API should use for securing the API.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnSouth.ServerCert,
	},
	cli.StringFlag{
		Name:        "sb-server-cacert",
		Usage:       "CA certificate that the OVN southbound API should use for securing the API.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnSouth.ServerCACert,
	},
	cli.StringFlag{
		Name:        "sb-client-privkey",
		Usage:       "Private key that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnSouth.ClientPrivKey,
	},
	cli.StringFlag{
		Name:        "sb-client-cert",
		Usage:       "Client certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		Destination: &cliConfig.OvnSouth.ClientCert,
	},
	cli.StringFlag{
		Name:        "sb-client-cacert",
		Usage:       "CA certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
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
}

func getOVSExternalID(name string) (string, error) {
	out, err := exec.Command("ovs-vsctl", "--if-exists", "get", "Open_vSwitch", ".", "external_ids:"+name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get OVS external_id %s: %v\n\t%s", name, err, out)
	}
	return strings.Trim(strings.TrimSpace(string(out)), "\""), nil
}

func fillDefaults(cfg *config, defaults *Defaults) {
	if defaults.OvnNorthAddress {
		addr, err := getOVSExternalID("ovn-nb")
		if err != nil {
			logrus.Debugf("Failed to get default OVN northbound database address: %v", err)
		} else if cfg.OvnNorth.Address == "" {
			cfg.OvnNorth.Address = addr
		}
	}

	if defaults.K8sAPIServer {
		addr, err := getOVSExternalID("k8s-api-server")
		if err != nil {
			logrus.Debugf("Failed to get default Kubernetes api-server address: %v", err)
		} else if cfg.Kubernetes.APIServer == "" {
			cfg.Kubernetes.APIServer = addr
		}
	}

	if defaults.K8sToken {
		token, err := getOVSExternalID("k8s-api-token")
		if err != nil {
			logrus.Debugf("Failed to get default Kubernetes API token: %v", err)
		} else if cfg.Kubernetes.Token == "" {
			cfg.Kubernetes.Token = token
		}
	}
}

func validateKubernetesConfig(cli, file *config) error {
	Kubernetes.Kubeconfig = getConfigStr(cli.Kubernetes.Kubeconfig, file.Kubernetes.Kubeconfig, "")
	if Kubernetes.Kubeconfig != "" && !pathExists(Kubernetes.Kubeconfig) {
		return fmt.Errorf("kubernetes kubeconfig file %q not found", Kubernetes.Kubeconfig)
	}
	Kubernetes.CACert = getConfigStr(cli.Kubernetes.CACert, file.Kubernetes.CACert, "")
	if Kubernetes.CACert != "" && !pathExists(Kubernetes.CACert) {
		return fmt.Errorf("kubernetes CA certificate file %q not found", Kubernetes.CACert)
	}
	Kubernetes.Token = getConfigStr(cli.Kubernetes.Token, file.Kubernetes.Token, "")
	Kubernetes.APIServer = getConfigStr(cli.Kubernetes.APIServer, file.Kubernetes.APIServer, "http://localhost:8443")
	url, err := url.Parse(Kubernetes.APIServer)
	if err != nil {
		return fmt.Errorf("kubernetes API server address %q invalid: %v", Kubernetes.APIServer, err)
	} else if url.Scheme != "https" && url.Scheme != "http" {
		return fmt.Errorf("kubernetes API server URL scheme %q invalid", url.Scheme)
	}

	if strings.HasPrefix(Kubernetes.APIServer, "https") && Kubernetes.CACert == "" {
		return fmt.Errorf("kubernetes API server %q scheme requires a CA certificate", Kubernetes.APIServer)
	}
	return nil
}

// InitConfig reads the config file and common command-line options and
// constructs the global config object from them.
func InitConfig(ctx *cli.Context, defaults *Defaults) error {
	var cfg config
	var err error
	var f *os.File

	// Error parsing a user-provided config file is a hard error
	configFile := ctx.String("config-file")
	if configFile != "" {
		f, err = os.Open(configFile)
		if err != nil {
			return fmt.Errorf("failed to open config file %s: %v", configFile, err)
		}
	} else {
		// Failure to find a default config file is not a hard error
		f, _ = os.Open("/etc/openvswitch/ovn_k8s.conf")
	}
	if f != nil {
		defer f.Close()

		// Parse ovn-k8s config file.
		if err = gcfg.ReadInto(&cfg, f); err != nil {
			return fmt.Errorf("failed to parse config file %s: %v", f.Name(), err)
		}
		logrus.Infof("Parsed config file %s", f.Name())
		logrus.Infof("Parsed config: %+v", cfg)

		if defaults != nil {
			fillDefaults(&cfg, defaults)
		}
	}

	// command-line > config file > default values
	Default.MTU = getConfigInt(cliConfig.Default.MTU, cfg.Default.MTU, 1400)
	Default.ConntrackZone = getConfigInt(cliConfig.Default.ConntrackZone, cfg.Default.ConntrackZone, 64000)

	Logging.File = getConfigStr(cliConfig.Logging.File, cfg.Logging.File, "")
	Logging.Level = getConfigInt(cliConfig.Logging.Level, cfg.Logging.Level, 4)

	CNI.ConfDir = getConfigStr(cliConfig.CNI.ConfDir, cfg.CNI.ConfDir, "/etc/cni/net.d")
	CNI.Plugin = getConfigStr(cliConfig.CNI.Plugin, cfg.CNI.Plugin, "ovn-k8s-cni-overlay")

	if err = validateKubernetesConfig(&cliConfig, &cfg); err != nil {
		return err
	}

	OvnNorth.ClientAuth, OvnNorth.ServerAuth, err = getOvnAuth(true, &cliConfig.OvnNorth, &cfg.OvnNorth)
	if err != nil {
		return err
	}

	OvnSouth.ClientAuth, OvnSouth.ServerAuth, err = getOvnAuth(false, &cliConfig.OvnSouth, &cfg.OvnSouth)
	if err != nil {
		return err
	}
	logrus.Debugf("Default config: %+v", Default)
	logrus.Debugf("Logging config: %+v", Logging)
	logrus.Debugf("CNI config: %+v", CNI)
	logrus.Debugf("Kubernetes config: %+v", Kubernetes)
	logrus.Debugf("OVN North config: %+v", OvnNorth)
	logrus.Debugf("OVN South config: %+v", OvnSouth)

	return nil
}

// OvnDBAuth describes an OVN database location and authentication method
type OvnDBAuth struct {
	URL     string
	PrivKey string
	Cert    string
	CACert  string

	server bool
	host   string
	port   string
	Scheme OvnDBScheme
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
func newOvnDBAuth(ctlCmd, urlString, privkey, cert, cacert string, server bool) (*OvnDBAuth, error) {
	if urlString == "" {
		if privkey != "" || cert != "" || cacert != "" {
			return nil, fmt.Errorf("certificate or key given; perhaps you mean to use the 'ssl' scheme?")
		}
		return &OvnDBAuth{
			server: server,
			Scheme: OvnDBSchemeUnix,
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

	var scheme OvnDBScheme
	switch {
	case url.Scheme == "ssl":
		scheme = OvnDBSchemeSSL
		if privkey == "" || cert == "" || cacert == "" {
			return nil, fmt.Errorf("must specify private key, certificate, and CA certificate for 'ssl' scheme")
		}
		if !server {
			// If the CA cert doesn't exist, try to fetch it from the peer
			if err = ensureCACert(ctlCmd, scheme, host, port, privkey, cert, cacert); err != nil {
				return nil, err
			}
		}
		if !pathExists(privkey) {
			return nil, fmt.Errorf("private key file %s not found", privkey)
		}
		if !pathExists(cert) {
			return nil, fmt.Errorf("certificate file %s not found", cert)
		}
		if !pathExists(cacert) {
			return nil, fmt.Errorf("CA certificate file %s not found", cacert)
		}
	case url.Scheme == "tcp":
		scheme = OvnDBSchemeTCP
		if privkey != "" || cert != "" || cacert != "" {
			return nil, fmt.Errorf("certificate or key given; perhaps you mean to use the 'ssl' scheme?")
		}
	default:
		return nil, fmt.Errorf("unknown OVN DB scheme %q", url.Scheme)
	}

	return &OvnDBAuth{
		URL:     urlString,
		PrivKey: privkey,
		Cert:    cert,
		CACert:  cacert,
		server:  server,
		host:    host,
		port:    port,
		Scheme:  scheme,
	}, nil
}

func ensureCACert(ctlCmd string, scheme OvnDBScheme, host, port, privkey, cert, cacert string) error {
	if _, err := os.Stat(cacert); err == nil || !os.IsNotExist(err) {
		return err
	}

	// If the CA cert doesn't exist, try to get it from the database
	cmdPath, err := exec.LookPath(ctlCmd)
	if err != nil {
		return err
	}

	args := []string{
		"--db=" + string(scheme) + ":" + host + ":" + port,
		"--timeout=5",
	}
	if scheme == OvnDBSchemeSSL {
		args = append(args, "--private-key="+privkey)
		args = append(args, "--certificate="+cert)
		args = append(args, "--bootstrap-ca-cert="+cacert)
	}
	args = append(args, "list", "nb_global")
	_ = exec.Command(cmdPath, args...).Run()
	if _, err = os.Stat(cacert); os.IsNotExist(err) {
		return fmt.Errorf("bootstapping %s CA certificate failed", cacert)
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

// SetDBServerAuth sets the authentication configuration for the OVN
// northbound or southbound database server
func (a *OvnDBAuth) SetDBServerAuth(ctlCmd, desc string) error {
	if a.Scheme == OvnDBSchemeUnix {
		return nil
	}

	out, err := exec.Command(ctlCmd, "set-connection", a.GetURL()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error setting %s API authentication: %v\n  %q", desc, err, string(out))
	}
	if a.Scheme == OvnDBSchemeSSL {
		out, err = exec.Command(ctlCmd, "set-ssl", a.PrivKey, a.Cert, a.CACert).CombinedOutput()
		if err != nil {
			return fmt.Errorf("Error setting SSL API certificates: %v\n  %q", err, string(out))
		}
	}
	return nil
}
