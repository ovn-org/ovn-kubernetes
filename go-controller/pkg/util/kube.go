package util

import (
	restclient "k8s.io/client-go/rest"
	certutil "k8s.io/client-go/util/cert"
)

// CreateConfig creates the restclient.Config object from token, ca file and the apiserver
// (similar to what is obtainable from kubeconfig file)
func CreateConfig(server, token, rootCAFile string) (*restclient.Config, error) {
	tlsClientConfig := restclient.TLSClientConfig{}
	if rootCAFile != "" {
		if _, err := certutil.NewPool(rootCAFile); err != nil {
			return nil, err
		}
		tlsClientConfig.CAFile = rootCAFile
	}

	return &restclient.Config{
		Host:            server,
		BearerToken:     token,
		TLSClientConfig: tlsClientConfig,
	}, nil
}
