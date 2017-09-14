package cluster

import (
	"fmt"
	"net"
	"net/url"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"k8s.io/client-go/tools/cache"
)

// OvnClusterController is the object holder for utilities meant for cluster management
type OvnClusterController struct {
	Kube                  kube.Interface
	masterSubnetAllocator *netutils.SubnetAllocator

	KubeServer       string
	CACert           string
	Token            string
	ClusterIPNet     *net.IPNet
	HostSubnetLength uint32

	NorthDBServerAuth *OvnDBAuth
	NorthDBClientAuth *OvnDBAuth

	SouthDBServerAuth *OvnDBAuth
	SouthDBClientAuth *OvnDBAuth

	StartNodeWatch func(handler cache.ResourceEventHandler)
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
