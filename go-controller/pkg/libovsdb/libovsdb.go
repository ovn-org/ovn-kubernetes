package libovsdb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"reflect"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"gopkg.in/fsnotify/fsnotify.v1"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
)

// newClient creates a new client object given the provided config
// the stopCh is required to ensure the goroutine for ssl cert
// update is not leaked
func newClient(cfg config.OvnAuthConfig, dbModel model.ClientDBModel, stopCh <-chan struct{}) (client.Client, error) {
	const connectTimeout time.Duration = types.OVSDBTimeout * 2
	logger := klogr.New()
	options := []client.Option{
		// Reading and parsing the DB after reconnect at scale can (unsurprisingly)
		// take longer than a normal ovsdb operation. Give it a bit more time so
		// we don't time out and enter a reconnect loop.
		client.WithReconnect(connectTimeout, &backoff.ZeroBackOff{}),
		client.WithLeaderOnly(true),
		client.WithLogger(&logger),
	}
	for _, endpoint := range strings.Split(cfg.GetURL(), ",") {
		options = append(options, client.WithEndpoint(endpoint))
	}
	var updateFn func(client.Client, <-chan struct{})
	if cfg.Scheme == config.OvnDBSchemeSSL {
		tlsConfig, err := createTLSConfig(cfg.Cert, cfg.PrivKey, cfg.CACert, cfg.CertCommonName)
		if err != nil {
			return nil, err
		}
		updateFn, err = newSSLKeyPairWatcherFunc(cfg.Cert, cfg.PrivKey, tlsConfig)
		if err != nil {
			return nil, err
		}
		options = append(options, client.WithTLSConfig(tlsConfig))
	}

	client, err := client.NewOVSDBClient(dbModel, options...)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	err = client.Connect(ctx)
	if err != nil {
		return nil, err
	}

	if updateFn != nil {
		go updateFn(client, stopCh)
	}

	return client, nil
}

// NewSBClient creates a new OVN Southbound Database client
func NewSBClient(stopCh <-chan struct{}) (client.Client, error) {
	return NewSBClientWithConfig(config.OvnSouth, stopCh)
}

// NewSBClientWithConfig creates a new OVN Southbound Database client with the provided configuration
func NewSBClientWithConfig(cfg config.OvnAuthConfig, stopCh <-chan struct{}) (client.Client, error) {
	dbModel, err := sbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	c, err := newClient(cfg, dbModel, stopCh)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	go func() {
		<-stopCh
		cancel()
	}()

	// Only Monitor Required SBDB tables to reduce memory overhead
	chassisPrivate := sbdb.ChassisPrivate{}
	_, err = c.Monitor(ctx,
		c.NewMonitor(
			// used by unidling controller
			client.WithTable(&sbdb.ControllerEvent{}),
			// used for gateway
			client.WithTable(&sbdb.MACBinding{}),
			// used by node sync
			client.WithTable(&sbdb.Chassis{}),
			// used by node sync, only interested in names
			client.WithTable(&chassisPrivate, &chassisPrivate.Name),
			// used for metrics
			client.WithTable(&sbdb.SBGlobal{}),
			// used for metrics
			client.WithTable(&sbdb.PortBinding{}),
			// used for hybrid-overlay
			client.WithTable(&sbdb.DatapathBinding{}),
			// used for dpu-host mode
			client.WithTable(&sbdb.Encap{}),
		),
	)
	if err != nil {
		c.Close()
		return nil, err
	}

	return c, nil
}

// NewNBClient creates a new OVN Northbound Database client
func NewNBClient(stopCh <-chan struct{}) (client.Client, error) {
	return NewNBClientWithConfig(config.OvnNorth, stopCh)
}

// NewNBClientWithConfig creates a new OVN Northbound Database client with the provided configuration
func NewNBClientWithConfig(cfg config.OvnAuthConfig, stopCh <-chan struct{}) (client.Client, error) {
	dbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	c, err := newClient(cfg, dbModel, stopCh)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	go func() {
		<-stopCh
		cancel()
	}()

	_, err = c.MonitorAll(ctx)
	if err != nil {
		c.Close()
		return nil, err
	}

	return c, nil
}

func createTLSConfig(certFile, privKeyFile, caCertFile, serverName string) (*tls.Config, error) {
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
	return tlsConfig, nil
}

// Watch TLS key/cert files, and update the ovndb tlsConfig Certificate.
// Call ovndbclient.Close() will disconnect underlying rpc2client connection.
// With ovndbclient initalized with reconnect flag, rcp2client will reconnct with new tlsConfig Certificate.
func newSSLKeyPairWatcherFunc(certFile, privKeyFile string, tlsConfig *tls.Config) (func(client.Client, <-chan struct{}), error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := watcher.Add(certFile); err != nil {
		return nil, err
	}
	if err := watcher.Add(privKeyFile); err != nil {
		return nil, err
	}
	fn := func(client client.Client, stopChan <-chan struct{}) {
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
					client.Disconnect()
					klog.Infof("TLS connection to %s force reconnected with new TLS config")
					// We do not call client.Connect() as reconnection is handled in the reconnect goroutine
				}
			case err, ok := <-watcher.Errors:
				if ok {
					klog.Errorf("Error watching for changes: %s", err)
				}
			case <-stopChan:
				err := watcher.Close()
				if err != nil {
					klog.Errorf("Error closing watcher: %s", err)
				}
				return
			}
		}
	}
	return fn, nil
}
