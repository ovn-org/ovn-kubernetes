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
func cudnGatewayRouterName(cudnName, nodeName string) string {
	return types.GWRouterPrefix + util.GetUserDefinedNetworkPrefix(types.CUDNPrefix+cudnName) + nodeName
}

// runOVNNbctlOnNode runs ovn-nbctl from the ovnkube-node pod on the given node.
func runOVNNbctlOnNode(k8sClient kubernetes.Interface, nodeName string, args ...string) (string, error) {
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
	hasNBOVSDBContainer := false
	for _, container := range nbPod.Spec.Containers {
		if container.Name == "nb-ovsdb" {
			hasNBOVSDBContainer = true
			break
		}
	}
	if !hasNBOVSDBContainer {
		return "", fmt.Errorf("ovnkube-node pod %s on node %s has no nb-ovsdb container", nbPod.Name, nodeName)
	}
	kubectlArgs := append([]string{"exec", nbPod.Name, "-c", "nb-ovsdb", "--", "ovn-nbctl"}, args...)
	return e2ekubectl.RunKubectl(ns, kubectlArgs...)
}

// cudnGRRoutesForNode returns the output of `ovn-nbctl lr-route-list` for
// the CUDN gateway router of the given CUDN on the given node.
func cudnGRRoutesForNode(k8sClient kubernetes.Interface, cudnName, nodeName string) (string, error) {
	return runOVNNbctlOnNode(k8sClient, nodeName, "lr-route-list", cudnGatewayRouterName(cudnName, nodeName))
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
