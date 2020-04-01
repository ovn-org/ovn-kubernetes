package util

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"

	goovn "github.com/ebay/go-ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"k8s.io/klog"
)

var OVNNBDBClient goovn.Client
var OVNSBDBClient goovn.Client

func InitOVNDBClients() error {
	var err error

	switch config.OvnNorth.Scheme {
	case config.OvnDBSchemeSSL:
		OVNNBDBClient, err = initGoOvnSslClient(config.OvnNorth.Cert,
			config.OvnNorth.PrivKey, config.OvnNorth.CACert,
			config.OvnNorth.GetURL())
		break
	case config.OvnDBSchemeTCP:
		OVNNBDBClient, err = initGoOvnTcpClient(config.OvnNorth.GetURL())
		break
	case config.OvnDBSchemeUnix:
		OVNNBDBClient, err = initGoOvnUnixClient(config.OvnNorth.GetURL())
		break
	default:
		klog.Errorf("Invalid db scheme: %s when initializing the OVN NB Client",
			config.OvnNorth.Scheme)
	}

	if err != nil {
		return fmt.Errorf("Couldn't initialize NBDB client: %s", err)
	}

	klog.Infof("Created OVN NB client with Scheme: %s", config.OvnNorth.Scheme)

	switch config.OvnSouth.Scheme {
	case config.OvnDBSchemeSSL:
		OVNSBDBClient, err = initGoOvnSslClient(config.OvnSouth.Cert,
			config.OvnSouth.PrivKey, config.OvnSouth.CACert,
			config.OvnSouth.GetURL())
		break
	case config.OvnDBSchemeTCP:
		OVNSBDBClient, err = initGoOvnTcpClient(config.OvnSouth.GetURL())
		break
	case config.OvnDBSchemeUnix:
		OVNSBDBClient, err = initGoOvnUnixClient(config.OvnSouth.GetURL())
		break
	default:
		klog.Errorf("Invalid db scheme: %s when initializing the OVN SB Client",
			config.OvnSouth.Scheme)
	}

	if err != nil {
		return fmt.Errorf("Couldn't initialize SBDB client: %s", err)
	}

	klog.Infof("Created OVN SB client with Scheme: %s", config.OvnSouth.Scheme)
	return nil
}

func initGoOvnSslClient(certFile, privKeyFile, caCertFile, address string) (goovn.Client, error) {
	cert, err := tls.LoadX509KeyPair(certFile, privKeyFile)
	if err != nil {
		return nil, fmt.Errorf("Error generating x509 certs for ovndbapi: %s", err)
	}
	caCert, err := ioutil.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("Error generating ca certs for ovndbapi: %s", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	ovndbclient, err := goovn.NewClient(&goovn.Config{
		Addr: address,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      caCertPool,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("Error creating SSL OVNDBClient for address %s: %s", address, err)
	}
	klog.Infof("Created OVNDB SSL client")
	return ovndbclient, nil
}

func initGoOvnTcpClient(address string) (goovn.Client, error) {
	ovndbclient, err := goovn.NewClient(&goovn.Config{
		Addr: address,
	})
	if err != nil {
		return nil, fmt.Errorf("Error creating TCP OVNDBClient for address %s: %s", address, err)
	}
	klog.Infof("Created OVNDB TCP client")
	return ovndbclient, nil
}

func initGoOvnUnixClient(address string) (goovn.Client, error) {
	ovndbclient, err := goovn.NewClient(&goovn.Config{
		Addr: address,
	})
	if err != nil {
		return nil, fmt.Errorf("Error creating OVNDBClient for address %s: %s", address, err)
	}
	klog.Infof("Created OVNDB UNIX client")
	return ovndbclient, nil
}
