package client

import (
	"crypto/tls"
	"net/url"
)

const (
	defaultTCPEndpoint  = "tcp:127.0.0.1:6640"
	defaultSSLEndpoint  = "ssl:127.0.0.1:6640"
	defaultUnixEndpoint = "unix:/var/run/openvswitch/ovsdb.sock"
)

type options struct {
	endpoints []string
	tlsConfig *tls.Config
}

type Option func(o *options) error

func newOptions(opts ...Option) (*options, error) {
	o := &options{}
	for _, opt := range opts {
		if err := opt(o); err != nil {
			return nil, err
		}
	}
	// if no endpoints are supplied, use the default unix socket
	if len(o.endpoints) == 0 {
		o.endpoints = []string{defaultUnixEndpoint}
	}
	return o, nil
}

// WithTLSConfig sets the tls.Config for use by the client
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *options) error {
		o.tlsConfig = cfg
		return nil
	}
}

// WithEndpoint sets the endpoint to be used by the client
// It can be used multiple times, and the first endpoint that
// successfully connects will be used.
// Endpoints are specified in OVSDB Connection Format
// For more details, see the ovsdb(7) man page
func WithEndpoint(endpoint string) Option {
	return func(o *options) error {
		ep, err := url.Parse(endpoint)
		if err != nil {
			return err
		}
		switch ep.Scheme {
		case UNIX:
			if len(ep.Path) == 0 {
				o.endpoints = append(o.endpoints, defaultUnixEndpoint)
				return nil
			}
		case TCP:
			if len(ep.Opaque) == 0 {
				o.endpoints = append(o.endpoints, defaultTCPEndpoint)
				return nil
			}
		case SSL:
			if len(ep.Opaque) == 0 {
				o.endpoints = append(o.endpoints, defaultSSLEndpoint)
				return nil
			}
		}
		o.endpoints = append(o.endpoints, endpoint)
		return nil
	}
}
