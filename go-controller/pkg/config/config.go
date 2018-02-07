package config

import (
	"os"

	"github.com/Sirupsen/logrus"
	gcfg "gopkg.in/gcfg.v1"
)

const (
	// ConfigFilePath is the config file path for more fine grained control.
	ConfigFilePath = "/etc/openvswitch/ovn_k8s.conf"
)

var (
	// MTU value used for the overlay networks.
	MTU = 1400
	// ConntrackZone affects only the gateway nodes, This value is used to track connections
	// that are initiated from the pods so that the reverse connections go back to the pods.
	// This represents the conntrack zone used for the conntrack flow rules.
	ConntrackZone = 64000
	// OvnMode can be overlay or underlay, At the moment only overlay networks are supported.
	OvnMode = "overlay"
	// OvnNB is the address of ovn northbound API server.
	OvnNB string
	// Scheme stores the scheme of OvnNB, eg. "ssl","tcp","unix".
	Scheme string
	// LogPath is the log path of ovn-k8s-cni-overlay binary.
	LogPath = "/var/log/openvswitch/ovn-k8s-cni-overlay.log"
	// UnixSocket is the unix socket path of ovn northbound database.
	UnixSocket = "/var/run/openvswitch/ovnnb_db.sock"
	// CniConfPath is the path of cni config file.
	CniConfPath = "/etc/cni/net.d"
	// CniLinkPath is the path of cni bin.
	CniLinkPath = "/opt/cni/bin"
	// CniPlugin is the name of cni
	CniPlugin = "ovn-k8s-cni-overlay"
	// NbctlPrivateKey is the private key for authenticating OVN DB.
	NbctlPrivateKey = "/etc/openvswitch/ovncontroller-privkey.pem"
	// NbctlCertificate is the certificate for authenticating OVN DB.
	NbctlCertificate = "/etc/openvswitch/ovncontroller-cert.pem"
	// NbctlCACert is the CA certificate for authenticating OVN DB.
	NbctlCACert = "/etc/openvswitch/ovnnb-ca.cert"
	// K8sCACertificate is k8s CA certificate.
	K8sCACertificate = ""
	// Rundir is run dir of OVS core utilities, between "/var/run/openvswitch" and "/usr/local/var/run/openvswitch".
	Rundir string
	// Logdir is log dir of OVS core utilities, between "/var/log/openvswitch" and "/usr/local/var/log/openvswitch".
	Logdir string
)

// Config is used for more fine grained control.
type Config struct {
	Default struct {
		// MTU value used for the overlay networks.
		MTU int `gcfg:"mtu"`
		// ConntrackZone affects only the gateway nodes, This value is used to track connections
		// that are initiated from the pods so that the reverse connections go back to the pods.
		// This represents the conntrack zone used for the conntrack flow rules.
		ConntrackZone int `gcfg:"conntrack-zone"`
		// OvnMode can be overlay or underlay, At the moment only overlay networks are supported.
		OvnMode string `gcfg:"ovn-mode"`
		// The following config values are used for the CNI plugin.
		LogPath     string `gcfg:"log-path"`
		UnixSocket  string `gcfg:"unix-socket"`
		CniConfPath string `gcfg:"cni-conf-path"`
		CniLinkPath string `gcfg:"cni-link-path"`
		CniPlugin   string `gcfg:"cni-plugin"`
		// OVN and K8S certificates are stored in the following options.
		PrivateKey       string `gcfg:"private-key"`
		Certificate      string `gcfg:"certificate"`
		CACert           string `gcfg:"ca-cert"`
		K8sCACertificate string `gcfg:"k8s-ca-certificate"`
		// We do not know how OVS core utilities have been installed. If those values are not set,
		// we try to do a best guess for rundir,logdir and choose between "/var/run/openvswitch","/var/log/openvswitch"
		// and "/usr/local/var/run/openvswitch","/usr/local/var/log/openvswitch".
		Rundir string `gcfg:"rundir"`
		Logdir string `gcfg:"logdir"`
	}
}

// FetchConfig fetch config file to override default values.
func FetchConfig() {
	// Open ovn-k8s config file.
	conf, err := os.Open(ConfigFilePath)
	if err != nil {
		logrus.Infof("Open configuration file %s error: %v, use default values", ConfigFilePath, err)
		return
	}
	defer conf.Close()

	// Parse ovn-k8s config file.
	var cfg Config
	err = gcfg.ReadInto(&cfg, conf)
	if err != nil {
		logrus.Warningf("Failed to parse ovn-k8s configure file: %v, use default values", err)
		return
	}
	logrus.Infof("Parsed ovn-k8s configure file: %v", cfg)

	if cfg.Default.MTU != 0 {
		MTU = cfg.Default.MTU
	}
	if cfg.Default.ConntrackZone != 0 {
		ConntrackZone = cfg.Default.ConntrackZone
	}
	if cfg.Default.OvnMode != "" {
		OvnMode = cfg.Default.OvnMode
	}
	if cfg.Default.LogPath != "" {
		LogPath = cfg.Default.LogPath
	}
	if cfg.Default.UnixSocket != "" {
		UnixSocket = cfg.Default.UnixSocket
	}
	if cfg.Default.CniConfPath != "" {
		CniConfPath = cfg.Default.CniConfPath
	}
	if cfg.Default.CniLinkPath != "" {
		CniLinkPath = cfg.Default.CniLinkPath
	}
	if cfg.Default.CniPlugin != "" {
		CniPlugin = cfg.Default.CniPlugin
	}
	if cfg.Default.PrivateKey != "" {
		NbctlPrivateKey = cfg.Default.PrivateKey
	}
	if cfg.Default.Certificate != "" {
		NbctlCertificate = cfg.Default.Certificate
	}
	if cfg.Default.CACert != "" {
		NbctlCACert = cfg.Default.CACert
	}
	if cfg.Default.K8sCACertificate != "" {
		K8sCACertificate = cfg.Default.K8sCACertificate
	}
	if cfg.Default.Rundir != "" {
		Rundir = cfg.Default.Rundir
	}
	if cfg.Default.Logdir != "" {
		Logdir = cfg.Default.Logdir
	}
}
