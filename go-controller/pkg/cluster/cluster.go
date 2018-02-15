package cluster

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"

	"github.com/sirupsen/logrus"
	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/client-go/kubernetes"
)

// OvnClusterController is the object holder for utilities meant for cluster management
type OvnClusterController struct {
	Kube                  kube.Interface
	watchFactory          *factory.WatchFactory
	masterSubnetAllocator *netutils.SubnetAllocator

	KubeServer            string
	CACert                string
	Token                 string
	ClusterIPNet          *net.IPNet
	ClusterServicesSubnet string
	HostSubnetLength      uint32

	GatewayInit      bool
	GatewayIntf      string
	GatewayBridge    string
	GatewayNextHop   string
	GatewaySpareIntf bool
	NodePortEnable   bool

	NorthDBServerAuth *OvnDBAuth
	NorthDBClientAuth *OvnDBAuth

	SouthDBServerAuth *OvnDBAuth
	SouthDBClientAuth *OvnDBAuth
}

// OvnDBScheme describes the OVN database connection transport method
type OvnDBScheme string

const (
	// OvnHostSubnet is the constant string representing the annotation key
	OvnHostSubnet = "ovn_host_subnet"

	// OvnDBSchemeSSL specifies SSL as the OVN database transport method
	OvnDBSchemeSSL OvnDBScheme = "ssl"
	// OvnDBSchemeTCP specifies TCP as the OVN database transport method
	OvnDBSchemeTCP OvnDBScheme = "tcp"
	// OvnDBSchemeUnix specifies Unix domains sockets as the OVN database transport method
	OvnDBSchemeUnix OvnDBScheme = "unix"
)

// NewClusterController creates a new controller for IP subnet allocation to
// a given resource type (either Namespace or Node)
func NewClusterController(kubeClient kubernetes.Interface, wf *factory.WatchFactory) *OvnClusterController {
	return &OvnClusterController{
		Kube:         &kube.Kube{KClient: kubeClient},
		watchFactory: wf,
	}
}

func setupOVN(nodeName, kubeServer, kubeToken string, northClientAuth, southClientAuth *OvnDBAuth) error {
	if _, err := url.Parse(kubeServer); err != nil {
		return fmt.Errorf("error parsing k8s server %q: %v", kubeServer, err)
	}

	nodeIP, err := netutils.GetNodeIP(nodeName)
	if err != nil {
		return fmt.Errorf("Failed to obtain local IP: %v", err)
	}

	args := []string{
		"set",
		"Open_vSwitch",
		".",
		"external_ids:ovn-encap-type=geneve",
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", nodeIP),
		fmt.Sprintf("external_ids:k8s-api-server=\"%s\"", kubeServer),
		fmt.Sprintf("external_ids:k8s-api-token=\"%s\"", kubeToken),
	}
	if northClientAuth.scheme != OvnDBSchemeUnix {
		args = append(args, fmt.Sprintf("external_ids:ovn-nb=\"%s\"", northClientAuth.GetURL()))
	}
	if southClientAuth.scheme != OvnDBSchemeUnix {
		args = append(args, fmt.Sprintf("external_ids:ovn-remote=\"%s\"", southClientAuth.GetURL()))
	}

	out, err := exec.Command("ovs-vsctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error setting OVS external IDs: %v\n  %q", err, string(out))
	}

	// Fetch config file to override default values.
	config.FetchConfig()

	// Update config globals that OVN exec utils use
	northClientAuth.SetConfig()

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
	scheme OvnDBScheme
}

// NewOvnDBAuth returns an OvnDBAuth object describing the connection to an
// OVN database, given a connection description string and authentication
// details
func NewOvnDBAuth(urlString, privkey, cert, cacert string, server bool) (*OvnDBAuth, error) {
	if urlString == "" {
		if privkey != "" || cert != "" || cacert != "" {
			return nil, fmt.Errorf("certificate or key given; perhaps you mean to use the 'ssl' scheme?")
		}
		return &OvnDBAuth{
			server: server,
			scheme: OvnDBSchemeUnix,
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
		scheme:  scheme,
	}, nil
}

// GetURL returns a URL suitable for passing to ovn-northd which describes the
// transport mechanism for connection to the database
func (a *OvnDBAuth) GetURL() string {
	// FIXME: support specific IP Addresses or non-default Unix socket paths
	if a.server {
		return fmt.Sprintf("p%s:%s", a.scheme, a.port)
	}
	return fmt.Sprintf("%s:%s:%s", a.scheme, a.host, a.port)
}

// SetConfig sets global config variables from an OvnDBAuth object
func (a *OvnDBAuth) SetConfig() {
	config.Scheme = string(a.scheme)
	config.OvnNB = a.GetURL()
	if a.scheme == "ssl" {
		config.NbctlPrivateKey = a.PrivKey
		config.NbctlCertificate = a.Cert
		config.NbctlCACert = a.CACert
		_, err := os.Stat(config.NbctlCACert)
		if os.IsNotExist(err) {
			logrus.Infof("No ovn-nbctl CA certificate found. " +
				"Attempting bootstrapping...")
			_, _, err = util.RunOVNNbctl("list", "nb_global")
			if err != nil {
				// An error is expected. But did bootstrapping succeed?"
				_, err = os.Stat(config.NbctlCACert)
				if os.IsNotExist(err) {
					logrus.Errorf("Bootstapping OVN NB's certificate failed")
				}
			}
		}
	}
}

// SetDBServerAuth sets the authentication configuration for the OVN
// northbound or southbound database server
func (a *OvnDBAuth) SetDBServerAuth(ctlCmd, desc string) error {
	if a.scheme == OvnDBSchemeUnix {
		return nil
	}

	out, err := exec.Command(ctlCmd, "set-connection", a.GetURL()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error setting %s API authentication: %v\n  %q", desc, err, string(out))
	}
	if a.scheme == OvnDBSchemeSSL {
		out, err = exec.Command(ctlCmd, "set-ssl", a.PrivKey, a.Cert, a.CACert).CombinedOutput()
		if err != nil {
			return fmt.Errorf("Error setting SSL API certificates: %v\n  %q", err, string(out))
		}
	}
	return nil
}
