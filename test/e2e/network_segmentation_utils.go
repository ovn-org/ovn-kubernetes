// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	"k8s.io/utils/net"
)

// cudnGatewayRouterName returns the name of the OVN gateway router for the
// given ClusterUserDefinedNetwork on the given node. The format is:
//
//	GR_cluster_udn_<sanitized-cudn-name>_<node>
//
// where the sanitization (dashes/slashes -> dots, trailing underscore)
// matches what the production code uses in go-controller/pkg/util/multi_network.go
// cudnNetworkName returns the OVN network name of a ClusterUserDefinedNetwork.
func cudnNetworkName(cudnName string) string {
	return types.CUDNPrefix + cudnName
}

func cudnGatewayRouterName(cudnName, nodeName string) string {
	return types.GWRouterPrefix + util.GetUserDefinedNetworkPrefix(types.CUDNPrefix+cudnName) + nodeName
}

// cudnGRRoutesForNode returns the output of `ovn-nbctl lr-route-list` for
// the CUDN gateway router of the given CUDN on the given node.
func cudnGRRoutesForNode(k8sClient kubernetes.Interface, cudnName, nodeName string) (string, error) {
	ns := deploymentconfig.Get().OVNKubernetesNamespace()
	nbPods, err := k8sClient.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to locate ovnkube-node pod on %s: %w", nodeName, err)
	}
	if len(nbPods.Items) == 0 {
		return "", fmt.Errorf("no ovnkube-node pod found on node %s", nodeName)
	}
	nbPod := nbPods.Items[0]
	if len(nbPod.Spec.Containers) == 0 {
		return "", fmt.Errorf("no containers found in ovnkube-node pod %s on node %s", nbPod.Name, nodeName)
	}
	nbContainerName := nbPod.Spec.Containers[0].Name
	if nbContainerName == "" {
		return "", fmt.Errorf("first container has no name in ovnkube-node pod %s on node %s", nbPod.Name, nodeName)
	}
	return e2ekubectl.RunKubectl(ns,
		"exec", nbPod.Name, "-c", nbContainerName, "--",
		"ovn-nbctl", "lr-route-list",
		cudnGatewayRouterName(cudnName, nodeName))
}

// nbctlOnNode runs ovn-nbctl with the given arguments in the ovnkube-node pod
// of the node, against the node's own northbound database.
func nbctlOnNode(k8sClient kubernetes.Interface, nodeName string, args ...string) (string, error) {
	ns := deploymentconfig.Get().OVNKubernetesNamespace()
	nbPods, err := k8sClient.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to locate ovnkube-node pod on %s: %w", nodeName, err)
	}
	if len(nbPods.Items) == 0 || len(nbPods.Items[0].Spec.Containers) == 0 {
		return "", fmt.Errorf("no ovnkube-node pod with containers found on node %s", nodeName)
	}
	nbPod := nbPods.Items[0]
	return e2ekubectl.RunKubectl(ns, append([]string{
		"exec", nbPod.Name, "-c", nbPod.Spec.Containers[0].Name, "--", "ovn-nbctl"}, args...)...)
}

// podIPsForUserDefinedPrimaryNetwork returns the v4 or v6 IPs for a pod on the UDN
func getPodAnnotationIPsForPrimaryNetworkByIPFamily(k8sClient kubernetes.Interface, podNamespace string, podName string, networkName string, family net.IPFamily) (string, error) {
	if networkName != "default" {
		networkName = namespacedName(podNamespace, networkName)
	}
	ipnets, err := getPodAnnotationIPsForAttachment(k8sClient, podNamespace, podName, networkName)
	if err != nil {
		return "", err
	}
	ipnet := getFirstCIDROfFamily(family, ipnets)
	if ipnet == nil {
		return "", nil
	}
	return ipnet.IP.String(), nil
}
