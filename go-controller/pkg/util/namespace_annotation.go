package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// Annotation used to enable/disable multicast in the namespace
	NsMulticastAnnotation = "k8s.ovn.org/multicast-enabled"
	// Annotations used by multiple external gateways feature
	RoutingExternalGWsAnnotation    = "k8s.ovn.org/routing-external-gws"
	RoutingNamespaceAnnotation      = "k8s.ovn.org/routing-namespaces"
	RoutingNetworkAnnotation        = "k8s.ovn.org/routing-network"
	BfdAnnotation                   = "k8s.ovn.org/bfd-enabled"
	ExternalGatewayPodIPsAnnotation = "k8s.ovn.org/external-gw-pod-ips"
	// Annotation for enabling ACL logging to controller's log file
	AclLoggingAnnotation    = "k8s.ovn.org/acl-logging"
	ActiveNetworkAnnotation = "k8s.ovn.org/active-network"
)

func UpdateExternalGatewayPodIPsAnnotation(k kube.Interface, namespace string, exgwIPs []string) error {
	exgwPodAnnotation := strings.Join(exgwIPs, ",")
	err := k.SetAnnotationsOnNamespace(namespace, map[string]interface{}{ExternalGatewayPodIPsAnnotation: exgwPodAnnotation})
	if err != nil {
		return fmt.Errorf("failed to add annotation %s/%v for namespace %s: %v", ExternalGatewayPodIPsAnnotation, exgwPodAnnotation, namespace, err)
	}
	return nil
}

func ParseRoutingExternalGWAnnotation(annotation string) (sets.Set[string], error) {
	ipTracker := sets.New[string]()
	if annotation == "" {
		return ipTracker, nil
	}
	for _, v := range strings.Split(annotation, ",") {
		parsedAnnotation := net.ParseIP(v)
		if parsedAnnotation == nil {
			return nil, fmt.Errorf("could not parse routing external gw annotation value %s", v)
		}
		if ipTracker.Has(parsedAnnotation.String()) {
			klog.Warningf("Duplicate IP detected in routing external gw annotation: %s", annotation)
			continue
		}
		ipTracker.Insert(parsedAnnotation.String())
	}
	return ipTracker, nil
}

func UpdateNamespaceActiveNetwork(k kubernetes.Interface, namespace *corev1.Namespace, network string) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": map[string]string{
				ActiveNetworkAnnotation: network,
			},
			"resourceVersion": namespace.ResourceVersion,
		},
	}

	patchData, err = json.Marshal(&patch)
	if err != nil {
		return fmt.Errorf("failed setting annotations on namespace %s: %w", namespace.Name, err)
	}

	_, err = k.CoreV1().Namespaces().Patch(context.TODO(), namespace.Name, types.StrategicMergePatchType, patchData, metav1.PatchOptions{}, "status")
	if err != nil {
		return fmt.Errorf("failed setting annotation on namespace %s: %w", namespace.Name, err)
	}
	return nil
}

func GetNamespaceActiveNetwork(namespace *corev1.Namespace) string {
	activeNetwork, ok := namespace.Annotations[ActiveNetworkAnnotation]
	if !ok {
		return ovntypes.DefaultNetworkName
	}

	return activeNetwork
}
