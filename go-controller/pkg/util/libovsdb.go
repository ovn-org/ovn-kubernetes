package util

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"math/rand"
	"reflect"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"gopkg.in/fsnotify/fsnotify.v1"
	"k8s.io/klog/v2"
)

type ModelClient struct {
	nbclient client.Client
}

func NewModelClient(nbclient client.Client) ModelClient {
	return ModelClient{nbclient: nbclient}
}

// OperationModel is a struct which uses reflection to determine and
// perform idempotent operations against NB DB
type OperationModel struct {
	// Model specifies the model to be created/updated/deleted. NOTE: when
	// specifying a named UUID (for the purpose of creating and referencing the
	// model against a ReferentialModel), DO NOT specify a named UUID with
	// special characters, this is not permitted...and difficult to debug.
	Model interface{}
	// ModelPredicate specifies the predicate at which the lookup for the model
	// is made. This is what defines if the object exists or not. Required
	// field.
	ModelPredicate interface{}
	// OnModelMutations specifies the mutations to be performed on the model if an
	// item exists. This is an optional field, and not specifying it means that
	// a create/update operation is to be performed.
	OnModelMutations func() []model.Mutation
	// OnModelUpdates specifies the model fields to be updated if an item exists.
	// This is an optional field, and not specifying it means that a
	// create/mutation operation is to be performed. If specified: ModelCreate
	// needs to be specified too, as that is used to demark which values each
	// field to be update will have.
	OnModelUpdates []interface{}
	// ExistingResult specifies the existing results used to determine the
	// existance/non-existance of the model.
	ExistingResult interface{}
}

// CreateOrUpdate performs a lookup of the Model given the ModelPredicate
// provided. If no ModelPredicate is provided: it uses a direct lookup of the
// Model in the cache, using the index of the Model. If a model exists it's
// either mutated or updated (depending on what the caller defines should be
// done). If the model does not exist it is created. The predicate assumes that
// only one object is retrieved for that predicate (since create/update/mutate
// is usually logically performed against one entity).
func (c *ModelClient) CreateOrUpdate(opModels ...OperationModel) ([]ovsdb.OperationResult, error) {
	ops := []ovsdb.Operation{}
	for _, opModel := range opModels {
		klog.V(5).Infof("Processing following opModel: %+v", opModel)
		if opModel.ModelPredicate != nil {
			if err := c.nbclient.WhereCache(opModel.ModelPredicate).List(opModel.ExistingResult); err != nil {
				return nil, fmt.Errorf("unable to list items for model, err: %v", err)
			}
			klog.V(5).Infof("Found following results in cache: %+v", opModel.ExistingResult)
			if reflect.ValueOf(opModel.ExistingResult).Elem().Len() == 0 {
				if opModel.Model != nil {
					o, err := c.nbclient.Create(reflect.ValueOf(opModel.Model).Interface())
					if err != nil {
						return nil, fmt.Errorf("unable to create model, err: %v", err)
					}
					klog.V(5).Infof("Create operations generated as: %+v", o)
					ops = append(ops, o...)
				}
			} else if reflect.ValueOf(opModel.ExistingResult).Elem().Len() == 1 {
				if opModel.OnModelMutations != nil {
					mutations := opModel.OnModelMutations()
					if mutations != nil {
						o, err := c.nbclient.Where(reflect.ValueOf(opModel.ExistingResult).Elem().Index(0).Addr().Interface()).Mutate(reflect.ValueOf(opModel.Model).Interface(), mutations...)
						if err != nil {
							return nil, fmt.Errorf("unable to update model, err: %v", err)
						}
						klog.V(5).Infof("Mutate operations generated as: %+v", o)
						ops = append(ops, o...)
					}
				}
				if len(opModel.OnModelUpdates) > 0 {
					o, err := c.nbclient.Where(reflect.ValueOf(opModel.ExistingResult).Elem().Index(0).Addr().Interface()).Update(reflect.ValueOf(opModel.Model).Interface(), opModel.OnModelUpdates...)
					if err != nil {
						return nil, fmt.Errorf("unable to update model, err: %v", err)
					}
					klog.V(5).Infof("Update operations generated as: %+v", o)
					ops = append(ops, o...)
				}
			} else {
				return nil, fmt.Errorf("multiple results found for model by given predicate")
			}
		} else if opModel.OnModelUpdates != nil {
			o, err := c.nbclient.Where(opModel.Model).Update(opModel.Model, opModel.OnModelUpdates...)
			if err != nil {
				if err == client.ErrNotFound {
					o, err = c.nbclient.Create(opModel.Model)
					if err != nil {
						return nil, fmt.Errorf("unable to create model, err: %v", err)
					}
					klog.V(5).Infof("Create operations generated as: %+v", o)
				} else {
					return nil, fmt.Errorf("unable to directly update model, err: %v", err)
				}
			} else {
				klog.V(5).Infof("Update operations generated as: %+v", o)
			}
			ops = append(ops, o...)
		} else if opModel.OnModelMutations != nil && opModel.OnModelMutations() != nil {
			o, err := c.nbclient.Where(opModel.Model).Mutate(opModel.OnModelMutations())
			if err != nil {
				if err == client.ErrNotFound {
					o, err = c.nbclient.Create(opModel.Model)
					if err != nil {
						return nil, fmt.Errorf("unable to create model, err: %v", err)
					}
					klog.V(5).Infof("Create operations generated as: %+v", o)
				} else {
					return nil, fmt.Errorf("unable to directly mutate object, err: %v", err)
				}
			} else {
				klog.V(5).Infof("Mutate operations generated as: %+v", o)
			}
			ops = append(ops, o...)
		}
	}
	if len(ops) > 0 {
		return c.transact(ops)
	}
	return nil, nil
}

// Delete deletes the Model found by the provided ModelPredicate. If a model is
// not found it does not do anything, if more than one is found it proceeds to
// updating each according to the options specified. It deletes the model by
// either destroying the reference to the model (if the Model provided depends
// on a reference to another) or by deleting the model directly, the logic is
// left up to the caller.
func (c *ModelClient) Delete(opModels ...OperationModel) error {
	ops := []ovsdb.Operation{}
	for _, opModel := range opModels {
		if opModel.ModelPredicate != nil {
			if err := c.nbclient.WhereCache(opModel.ModelPredicate).List(opModel.ExistingResult); err != nil {
				return fmt.Errorf("unable to list items for model, err: %v", err)
			}
			for i := 0; i < reflect.ValueOf(opModel.ExistingResult).Elem().Len(); i++ {
				if opModel.OnModelMutations != nil {
					mutations := opModel.OnModelMutations()
					if mutations != nil {
						o, err := c.nbclient.Where(reflect.ValueOf(opModel.ExistingResult).Elem().Index(i).Addr().Interface()).Mutate(reflect.ValueOf(opModel.Model).Interface(), mutations...)
						if err != nil {
							return fmt.Errorf("unable to perform parent model mutations, err: %v", err)
						}
						klog.V(5).Infof("Delete mutate operations generated as: %+v", o)
						ops = append(ops, o...)
					}
				} else if opModel.Model != nil {
					o, err := c.nbclient.Where(reflect.ValueOf(opModel.ExistingResult).Elem().Index(i).Addr().Interface()).Delete()
					if err != nil {
						return fmt.Errorf("unable to delete model, err: %v", err)
					}
					klog.V(5).Infof("Delete operations generated as: %+v", o)
					ops = append(ops, o...)
				}
			}
		} else if opModel.OnModelMutations != nil && opModel.OnModelMutations() != nil {
			o, err := c.nbclient.Where(opModel.Model).Mutate(opModel.Model, opModel.OnModelMutations()...)
			if err != nil {
				return fmt.Errorf("unable to directly mutate model for delete, err: %v", err)
			}
			ops = append(ops, o...)
		} else if opModel.Model != nil {
			o, err := c.nbclient.Where(opModel.Model).Delete()
			if err != nil {
				return fmt.Errorf("unable to directly delete model, err: %v", err)
			}
			ops = append(ops, o...)
		}
	}
	if len(ops) > 0 {
		_, err := c.transact(ops)
		return err
	}
	return nil
}

func (c *ModelClient) transact(ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	res, err := c.nbclient.Transact(ops...)
	if err != nil {
		return nil, fmt.Errorf("unable to transact operations, err: %v", err)
	}
	if opErrors, err := ovsdb.CheckOperationResults(res, ops); err != nil {
		return nil, fmt.Errorf("unable to validate operations, ops: %+v, opErrors: %+v, err: %v", ops, opErrors, err)
	}
	return res, nil
}

func SliceHasStringItem(slice []string, item string) bool {
	for _, i := range slice {
		if i == item {
			return true
		}
	}
	return false
}

const (
	letterBytes   = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

var (
	n   = 5
	src = rand.NewSource(time.Now().UnixNano())
)

// GenerateNamedUUID generated a random named UUID for OVS DB server. The
// strings length if defined by `n`. The body of this function is copied from:
// https://stackoverflow.com/a/31832326
func GenerateNamedUUID() string {
	sb := strings.Builder{}
	sb.Grow(n)
	for i, cache, remain := n-1, src.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = src.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			sb.WriteByte(letterBytes[idx])
			i--
		}
		cache >>= letterIdxBits
		remain--
	}
	return sb.String()
}

// newClient creates a new client object given the provided config
// the stopCh is required to ensure the goroutine for ssl cert
// update is not leaked
func newClient(cfg config.OvnAuthConfig, dbModel *model.DBModel, stopCh <-chan struct{}) (client.Client, error) {
	options := []client.Option{
		client.WithReconnect(500*time.Millisecond, &backoff.ZeroBackOff{}),
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

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
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

// NewSBClient creates a new OVN Southbound Database client with the provided configuration
func NewSBClientWithConfig(cfg config.OvnAuthConfig, stopCh <-chan struct{}) (client.Client, error) {
	dbModel, err := sbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	return newClient(cfg, dbModel, stopCh)
}

// NewNBClient creates a new OVN Northbound Database client
func NewNBClient(stopCh <-chan struct{}) (client.Client, error) {
	return NewNBClientWithConfig(config.OvnNorth, stopCh)
}

// NewNBClient creates a new OVN Northbound Database client with the provided configuration
func NewNBClientWithConfig(cfg config.OvnAuthConfig, stopCh <-chan struct{}) (client.Client, error) {
	dbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	c, err := newClient(cfg, dbModel, stopCh)
	if err != nil {
		return nil, err
	}

	_, err = c.MonitorAll()
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
