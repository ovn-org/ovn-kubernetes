package util

import (
	"fmt"
	"strings"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/cert"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

// NewClientset creates a Kubernetes clientset from either a kubeconfig,
// TLS properties, or an apiserver URL
func NewClientset(conf *config.KubernetesConfig) (*kubernetes.Clientset, error) {
	var kconfig *rest.Config
	var err error

	if conf.Kubeconfig != "" {
		// uses the current context in kubeconfig
		kconfig, err = clientcmd.BuildConfigFromFlags("", conf.Kubeconfig)
	} else if strings.HasPrefix(conf.APIServer, "https") {
		if conf.APIServer == "" || conf.Token == "" {
			return nil, fmt.Errorf("TLS-secured apiservers require token and CA certificate")
		}
		kconfig = &rest.Config{
			Host:        conf.APIServer,
			BearerToken: conf.Token,
		}
		if conf.CACert != "" {
			if _, err := cert.NewPool(conf.CACert); err != nil {
				return nil, err
			}
			kconfig.TLSClientConfig = rest.TLSClientConfig{CAFile: conf.CACert}
		}
	} else if strings.HasPrefix(conf.APIServer, "http") {
		kconfig, err = clientcmd.BuildConfigFromFlags(conf.APIServer, "")
	} else {
		// Assume we are running from a container managed by kubernetes
		// and read the apiserver address and tokens from the
		// container's environment.
		kconfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}

	kconfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	kconfig.ContentType = "application/vnd.kubernetes.protobuf"

	return kubernetes.NewForConfig(kconfig)
}

// IsClusterIPSet checks if the service is an headless service or not
func IsClusterIPSet(service *kapi.Service) bool {
	return service.Spec.ClusterIP != kapi.ClusterIPNone && service.Spec.ClusterIP != ""
}

// ServiceTypeHasClusterIP checks if the service has an associated ClusterIP or not
func ServiceTypeHasClusterIP(service *kapi.Service) bool {
	return service.Spec.Type == kapi.ServiceTypeClusterIP || service.Spec.Type == kapi.ServiceTypeNodePort || service.Spec.Type == kapi.ServiceTypeLoadBalancer
}

// ServiceTypeHasNodePort checks if the service has an associated NodePort or not
func ServiceTypeHasNodePort(service *kapi.Service) bool {
	return service.Spec.Type == kapi.ServiceTypeNodePort || service.Spec.Type == kapi.ServiceTypeLoadBalancer
}

func validateOVNConfigEndpoint(ep *kapi.Endpoints) bool {
	return len(ep.Subsets) == 1 && len(ep.Subsets[0].Ports) == 2 && len(ep.Subsets[0].Addresses) > 0
}

// ExtractDbRemotesFromEndpoint extracts the DB endpoints
func ExtractDbRemotesFromEndpoint(ep *kapi.Endpoints) ([]string, int32, int32, error) {
	var nbDBPort int32
	var sbDBPort int32
	var masterIPList []string

	if !validateOVNConfigEndpoint(ep) {
		return masterIPList, nbDBPort, sbDBPort, fmt.Errorf("endpoint %s is not in the right format to configure OVN", ep.Name)
	}

	for _, ovnDB := range ep.Subsets[0].Ports {
		if ovnDB.Name == "south" {
			sbDBPort = ovnDB.Port
		} else if ovnDB.Name == "north" {
			nbDBPort = ovnDB.Port
		}
	}
	for _, address := range ep.Subsets[0].Addresses {
		masterIPList = append(masterIPList, address.IP)
	}

	return masterIPList, sbDBPort, nbDBPort, nil
}
