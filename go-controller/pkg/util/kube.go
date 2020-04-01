package util

import (
	"encoding/json"
	"fmt"
	"strings"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/cert"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
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

// GetNodeIP extracts the ip address from the node status in the  API
func GetNodeIP(node *kapi.Node) (string, error) {
	for _, addr := range node.Status.Addresses {
		if addr.Type == kapi.NodeInternalIP {
			return addr.Address, nil
		}
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == kapi.NodeExternalIP {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("%s doesn't have an address with type %s or %s", node.GetName(),
		kapi.NodeInternalIP, kapi.NodeExternalIP)
}

// GetNodeHostame extracts the hostname from the node status in the API
func GetNodeHostname(node *kapi.Node) (string, error) {
	for _, addr := range node.Status.Addresses {
		if addr.Type == kapi.NodeHostName {
			return addr.Address, nil
		}
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == kapi.NodeExternalDNS {
			return addr.Address, nil
		}
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == kapi.NodeInternalDNS {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("%s doesn't have an address with type %s, %s or %s", node.GetName(),
		kapi.NodeHostName, kapi.NodeExternalDNS, kapi.NodeInternalDNS)
}

const (
	// DefNetworkAnnotation is the pod annotation for the cluster-wide default network
	DefNetworkAnnotation = "v1.multus-cni.io/default-network"

	// NetworkAttachmentAnnotation is the pod annotation for network-attachment-definition
	NetworkAttachmentAnnotation = "k8s.v1.cni.cncf.io/networks"
)

// GetPodNetSelAnnotation returns the pod's Network Attachment Selection Annotation either for
// the cluster-wide default network or the additional networks.
//
// This function is a simplified version of parsePodNetworkAnnotation() function in multus-cni
// repository. We need to revisit once there is a library that we can share.
//
// Note that the changes below is based on following assumptions, which is true today.
// - a pod's default network is OVN managed
func GetPodNetSelAnnotation(pod *kapi.Pod, netAttachAnnot string) ([]*types.NetworkSelectionElement, error) {
	var networkAnnotation string
	var networks []*types.NetworkSelectionElement

	networkAnnotation = pod.Annotations[netAttachAnnot]
	if networkAnnotation == "" {
		// nothing special to do
		return nil, nil
	}

	// it is possible the default network is defined in the form of comma-delimited list of
	// network attachment resource names (i.e. list of <namespace>/<network name>@<ifname>), but
	// we are only interested in the NetworkSelectionElement json form that has custom MAC/IP
	if json.Valid([]byte(networkAnnotation)) {
		if err := json.Unmarshal([]byte(networkAnnotation), &networks); err != nil {
			return nil, fmt.Errorf("failed to parse pod's net-attach-definition JSON %q: %v", networkAnnotation, err)
		}
	} else {
		// nothing special to do
		return nil, nil
	}

	return networks, nil
}
