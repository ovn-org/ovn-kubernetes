package util

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"reflect"

	"io/ioutil"

	goovn "github.com/ebay/go-ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"gopkg.in/fsnotify/fsnotify.v1"
	"k8s.io/klog/v2"
)

func NewOVNNBClient() (goovn.Client, error) {
	var (
		err      error
		nbClient goovn.Client
	)

	switch config.OvnNorth.Scheme {
	case config.OvnDBSchemeSSL:
		nbClient, err = initGoOvnSslClient(config.OvnNorth.Cert,
			config.OvnNorth.PrivKey, config.OvnNorth.CACert,
			config.OvnNorth.GetURL(), goovn.DBNB, config.OvnNorth.CertCommonName)
	case config.OvnDBSchemeTCP:
		nbClient, err = initGoOvnTcpClient(config.OvnNorth.GetURL(), goovn.DBNB)
	case config.OvnDBSchemeUnix:
		nbClient, err = initGoOvnUnixClient(config.OvnNorth.GetURL(), goovn.DBNB)
	default:
		err = fmt.Errorf("invalid db scheme: %s when initializing the OVN NB Client",
			config.OvnNorth.Scheme)
	}

	if err != nil {
		return nil, fmt.Errorf("couldn't initialize NBDB client: %s", err)
	}

	klog.Infof("Created OVN NB client with Scheme: %s", config.OvnNorth.Scheme)
	return nbClient, nil
}

func NewOVNSBClient() (goovn.Client, error) {
	var (
		err      error
		sbClient goovn.Client
	)

	switch config.OvnSouth.Scheme {
	case config.OvnDBSchemeSSL:
		sbClient, err = initGoOvnSslClient(config.OvnSouth.Cert,
			config.OvnSouth.PrivKey, config.OvnSouth.CACert,
			config.OvnSouth.GetURL(), goovn.DBSB, config.OvnSouth.CertCommonName)
	case config.OvnDBSchemeTCP:
		sbClient, err = initGoOvnTcpClient(config.OvnSouth.GetURL(), goovn.DBSB)
	case config.OvnDBSchemeUnix:
		sbClient, err = initGoOvnUnixClient(config.OvnSouth.GetURL(), goovn.DBSB)
	default:
		err = fmt.Errorf("invalid db scheme: %s when initializing the OVN SB Client",
			config.OvnSouth.Scheme)
	}

	if err != nil {
		return nil, fmt.Errorf("couldn't initialize SBDB client: %s", err)
	}

	klog.Infof("Created OVN SB client with Scheme: %s", config.OvnSouth.Scheme)
	return sbClient, nil
}

func initGoOvnSslClient(certFile, privKeyFile, caCertFile, address, db, serverName string) (goovn.Client, error) {
	cert, err := tls.LoadX509KeyPair(certFile, privKeyFile)
	if err != nil {
		return nil, fmt.Errorf("error generating x509 certs for ovndbapi: %s", err)
	}
	caCert, err := ioutil.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("error generating ca certs for ovndbapi: %s", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
		ServerName:   serverName,
	}
	tlsConfig.BuildNameToCertificate()
	ovndbclient, err := goovn.NewClient(&goovn.Config{
		Db:        db,
		Addr:      address,
		TLSConfig: tlsConfig,
		Reconnect: true,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating SSL OVNDBClient for database %s at address %s: %s", db, address, err)
	}
	if err = updateSslKeyPair(db, certFile, privKeyFile, tlsConfig, ovndbclient); err != nil {
		return nil, fmt.Errorf("error watching SSL OVNDBClient for database %s cert/key files: %s", db, err)
	}

	klog.Infof("Created OVNDB SSL client for db: %s", db)
	return ovndbclient, nil
}

// Watch TLS key/cert files, and update the ovndb tlsConfig Certificate.
// Call ovndbclient.Close() will disconnect underlying rpc2client connection.
// With ovndbclient initalized with reconnect flag, rcp2client will reconnct with new tlsConfig Certificate.
func updateSslKeyPair(ovndb, certFile, privKeyFile string, tlsConfig *tls.Config, ovndbclient goovn.Client) error {

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if ok && event.Op&(fsnotify.Write|fsnotify.Remove) != 0 {
					cert, err := tls.LoadX509KeyPair(certFile, privKeyFile)
					if err != nil {
						klog.Infof("Cannot load new cert with cert %s key %s err %s", certFile, privKeyFile, err)
						continue
					}
					if reflect.DeepEqual(tlsConfig.Certificates, []tls.Certificate{cert}) {
						klog.Infof("TLS update already finished")
						continue
					}
					tlsConfig.Certificates = []tls.Certificate{cert}
					err = ovndbclient.Close()
					if err != nil {
						klog.Errorf("Cannot close %s connection: %s", ovndb, err)
						continue
					}
					klog.Infof("TLS connection to %s force reconnected with new tlsconfig", ovndb)
				}
			case err, ok := <-watcher.Errors:
				if ok {
					klog.Errorf("Error watching for changes: %s", err)
				}
			}
		}
	}()

	if err := watcher.Add(certFile); err != nil {
		return err
	}
	if err := watcher.Add(privKeyFile); err != nil {
		return err
	}
	return nil
}

func initGoOvnTcpClient(address, db string) (goovn.Client, error) {
	ovndbclient, err := goovn.NewClient(&goovn.Config{
		Db:        db,
		Addr:      address,
		Reconnect: true,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating TCP OVNDBClient for address %s: %s", address, err)
	}
	klog.Infof("Created OVNDB TCP client for db: %s", db)
	return ovndbclient, nil
}

func initGoOvnUnixClient(address, db string) (goovn.Client, error) {
	ovndbclient, err := goovn.NewClient(&goovn.Config{
		Db:        db,
		Addr:      address,
		Reconnect: true,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating UNIX OVNDBClient for address %s: %s", address, err)
	}
	klog.Infof("Created OVNDB UNIX client for db: %s", db)
	return ovndbclient, nil
}

// OvnNBLSPDel deletes the given logical switch port using the go-ovn library
func OvnNBLSPDel(nbClient goovn.Client, logicalPort string) error {
	cmd, err := nbClient.LSPDel(logicalPort)
	if err == nil {
		if err = nbClient.Execute(cmd); err != nil {
			return fmt.Errorf("error while deleting logical port: %s, %v", logicalPort, err)
		}
	} else if err != goovn.ErrorNotFound {
		return fmt.Errorf(err.Error())
	}
	return nil
}
