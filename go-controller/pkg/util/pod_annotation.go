package util

import (
	"fmt"

	netattachdefapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netattachdefutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"

	v1 "k8s.io/api/core/v1"
)

const (
	// DefNetworkAnnotation is the pod annotation for the cluster-wide default network
	DefNetworkAnnotation = "v1.multus-cni.io/default-network"
)

// GetK8sPodDefaultNetwork get pod default network from annotations
func GetK8sPodDefaultNetwork(pod *v1.Pod) (*netattachdefapi.NetworkSelectionElement, error) {
	var netAnnot string

	netAnnot, ok := pod.Annotations[DefNetworkAnnotation]
	if !ok {
		return nil, nil
	}

	networks, err := netattachdefutils.ParseNetworkAnnotation(netAnnot, pod.Namespace)
	if err != nil {
		return nil, fmt.Errorf("GetK8sPodDefaultNetwork: failed to parse CRD object: %v", err)
	}
	if len(networks) > 1 {
		return nil, fmt.Errorf("GetK8sPodDefaultNetwork: more than one default network is specified: %s", netAnnot)
	}

	if len(networks) == 1 {
		return networks[0], nil
	}

	return nil, nil
}

// GetK8sPodAllNetworks get pod's all network NetworkSelectionElement from k8s.v1.cni.cncf.io/networks annotation
func GetK8sPodAllNetworks(pod *v1.Pod) ([]*netattachdefapi.NetworkSelectionElement, error) {
	networks, err := netattachdefutils.ParsePodNetworkAnnotation(pod)
	if err != nil {
		if _, ok := err.(*netattachdefapi.NoK8sNetworkError); !ok {
			return nil, fmt.Errorf("failed to get all NetworkSelectionElements for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		networks = []*netattachdefapi.NetworkSelectionElement{}
	}
	return networks, nil
}
