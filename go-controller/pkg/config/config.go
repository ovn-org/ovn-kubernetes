package config

import (
	"os"

	"github.com/sirupsen/logrus"
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
	// OvnNB is the address of ovn northbound API server.
	OvnNB string
	// Scheme stores the scheme of OvnNB, eg. "ssl","tcp","unix".
	Scheme string
	// LogPath is the log path of ovn-k8s-cni-overlay binary.
	LogPath = "/var/log/openvswitch/ovn-k8s-cni-overlay.log"
	// CniConfPath is the path of cni config file.
	CniConfPath = "/etc/cni/net.d"
	// CniPlugin is the name of cni
	CniPlugin = "ovn-k8s-cni-overlay"
	// NbctlPrivateKey is the private key for authenticating OVN DB.
	NbctlPrivateKey = "/etc/openvswitch/ovncontroller-privkey.pem"
	// NbctlCertificate is the certificate for authenticating OVN DB.
	NbctlCertificate = "/etc/openvswitch/ovncontroller-cert.pem"
	// NbctlCACert is the CA certificate for authenticating OVN DB.
	NbctlCACert = "/etc/openvswitch/ovnnb-ca.cert"
	// K8sCACertificate is k8s CA certificate.
	K8sCACertificate = "/etc/openvswitch/k8s-ca.crt"
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
		// The following config values are used for the CNI plugin.
		CniConfPath string `gcfg:"cni-conf-path"`
		CniPlugin   string `gcfg:"cni-plugin"`
		// OVN and K8S certificates are stored in the following options.
		PrivateKey       string `gcfg:"private-key"`
		Certificate      string `gcfg:"certificate"`
		CACert           string `gcfg:"ca-cert"`
		K8sCACertificate string `gcfg:"k8s-ca-certificate"`
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
	if cfg.Default.CniConfPath != "" {
		CniConfPath = cfg.Default.CniConfPath
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
}
